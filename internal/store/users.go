package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// bcryptCost is the work factor for password hashing. The default is a
// deliberate trade-off; raising it slows logins for everyone.
const bcryptCost = bcrypt.DefaultCost

// ErrBadPassword is returned when a password does not match the stored hash.
var ErrBadPassword = errors.New("password does not match")

// HashPassword returns the bcrypt hash of a plaintext password.
func HashPassword(plain string) (string, error) {
	if strings.TrimSpace(plain) == "" {
		return "", errors.New("password must not be empty")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("hashing password: %w", err)
	}
	return string(hash), nil
}

// CheckPassword verifies a plaintext password against a stored hash.
func CheckPassword(hash, plain string) error {
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)); err != nil {
		return ErrBadPassword
	}
	return nil
}

// UserRepo stores panel accounts.
type UserRepo struct{ db *sql.DB }

const userColumns = `id, email, password_hash, role, theme, totp_secret, blocked,
	max_servers, max_ram_mb, max_disk_mb, created_at, updated_at`

// Create inserts a user, filling in its id and timestamps.
func (r *UserRepo) Create(ctx context.Context, u *User) error {
	if u.ID == "" {
		u.ID = NewID()
	}
	if u.Role == "" {
		u.Role = RoleUser
	}
	if u.Theme == "" {
		u.Theme = "system"
	}
	now := time.Now().UTC()
	u.CreatedAt, u.UpdatedAt = now, now

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO users (`+userColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.Email, u.PasswordHash, u.Role, u.Theme, u.TOTPSecret, u.Blocked,
		u.MaxServers, u.MaxRAMMb, u.MaxDiskMb, formatTime(u.CreatedAt), formatTime(u.UpdatedAt))
	if err != nil {
		if isUniqueViolation(err, "idx_users_email", "users.email") {
			return ErrEmailTaken
		}
		return fmt.Errorf("creating user %s: %w", u.Email, err)
	}
	return nil
}

// GetByID returns a user by id.
func (r *UserRepo) GetByID(ctx context.Context, id string) (*User, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id)
	return scanUser(row)
}

// GetByEmail returns a user by email, compared case-insensitively because
// addresses are not case-sensitive in practice and users type them either way.
func (r *UserRepo) GetByEmail(ctx context.Context, email string) (*User, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE lower(email) = lower(?)`, email)
	return scanUser(row)
}

// List returns users ordered by creation time.
func (r *UserRepo) List(ctx context.Context, limit int) ([]*User, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+userColumns+` FROM users ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("listing users: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var users []*User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// Count reports how many users exist. Used to decide whether the first-run
// admin still needs creating.
func (r *UserRepo) Count(ctx context.Context) (int, error) {
	var n int
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting users: %w", err)
	}
	return n, nil
}

// Update writes every mutable field of u.
func (r *UserRepo) Update(ctx context.Context, u *User) error {
	u.UpdatedAt = time.Now().UTC()

	res, err := r.db.ExecContext(ctx, `
		UPDATE users SET
			email = ?, password_hash = ?, role = ?, theme = ?, totp_secret = ?,
			blocked = ?, max_servers = ?, max_ram_mb = ?, max_disk_mb = ?, updated_at = ?
		WHERE id = ?`,
		u.Email, u.PasswordHash, u.Role, u.Theme, u.TOTPSecret, u.Blocked,
		u.MaxServers, u.MaxRAMMb, u.MaxDiskMb, formatTime(u.UpdatedAt), u.ID)
	if err != nil {
		if isUniqueViolation(err, "idx_users_email", "users.email") {
			return ErrEmailTaken
		}
		return fmt.Errorf("updating user %s: %w", u.ID, err)
	}
	return checkAffected(res, "user", u.ID)
}

// SetTheme stores the user's theme choice.
func (r *UserRepo) SetTheme(ctx context.Context, userID, theme string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE users SET theme = ?, updated_at = ? WHERE id = ?`,
		theme, formatTime(time.Now()), userID)
	if err != nil {
		return fmt.Errorf("setting theme for user %s: %w", userID, err)
	}
	return checkAffected(res, "user", userID)
}

// Delete removes a user. Tokens, servers and themes cascade.
func (r *UserRepo) Delete(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting user %s: %w", id, err)
	}
	return checkAffected(res, "user", id)
}

// rowScanner covers both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanUser(row rowScanner) (*User, error) {
	var (
		u         User
		blocked   int
		createdAt string
		updatedAt string
	)
	err := row.Scan(&u.ID, &u.Email, &u.PasswordHash, &u.Role, &u.Theme, &u.TOTPSecret,
		&blocked, &u.MaxServers, &u.MaxRAMMb, &u.MaxDiskMb, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scanning user: %w", err)
	}

	u.Blocked = blocked != 0
	if u.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, fmt.Errorf("parsing user created_at: %w", err)
	}
	if u.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, fmt.Errorf("parsing user updated_at: %w", err)
	}
	return &u, nil
}

// checkAffected turns a no-op update into ErrNotFound, so callers can tell
// "nothing changed" from "no such row".
func checkAffected(res sql.Result, kind, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("checking affected rows for %s %s: %w", kind, id, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
