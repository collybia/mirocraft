package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// CustomThemeRepo stores user-authored themes: overrides of the CSS variable
// contract on top of a built-in base.
type CustomThemeRepo struct{ db *sql.DB }

const customThemeColumns = `id, user_id, name, base, vars, created_at, updated_at`

// Create inserts a custom theme.
func (r *CustomThemeRepo) Create(ctx context.Context, t *CustomTheme) error {
	if t.ID == "" {
		t.ID = NewID()
	}
	if t.Base == "" {
		t.Base = "dark"
	}
	now := time.Now().UTC()
	t.CreatedAt, t.UpdatedAt = now, now

	vars, err := encodeVars(t.Vars)
	if err != nil {
		return err
	}

	_, err = r.db.ExecContext(ctx, `
		INSERT INTO custom_themes (`+customThemeColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.UserID, t.Name, t.Base, vars,
		formatTime(t.CreatedAt), formatTime(t.UpdatedAt))
	if err != nil {
		return fmt.Errorf("creating theme %s: %w", t.Name, err)
	}
	return nil
}

// GetByID returns a theme by id.
func (r *CustomThemeRepo) GetByID(ctx context.Context, id string) (*CustomTheme, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+customThemeColumns+` FROM custom_themes WHERE id = ?`, id)
	return scanCustomTheme(row)
}

// ListByUser returns a user's themes.
func (r *CustomThemeRepo) ListByUser(ctx context.Context, userID string) ([]*CustomTheme, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+customThemeColumns+` FROM custom_themes WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, fmt.Errorf("listing themes for user %s: %w", userID, err)
	}
	defer func() { _ = rows.Close() }()

	var themes []*CustomTheme
	for rows.Next() {
		t, err := scanCustomTheme(rows)
		if err != nil {
			return nil, err
		}
		themes = append(themes, t)
	}
	return themes, rows.Err()
}

// Update writes a theme's mutable fields.
func (r *CustomThemeRepo) Update(ctx context.Context, t *CustomTheme) error {
	t.UpdatedAt = time.Now().UTC()

	vars, err := encodeVars(t.Vars)
	if err != nil {
		return err
	}

	res, err := r.db.ExecContext(ctx, `
		UPDATE custom_themes SET name = ?, base = ?, vars = ?, updated_at = ?
		WHERE id = ?`,
		t.Name, t.Base, vars, formatTime(t.UpdatedAt), t.ID)
	if err != nil {
		return fmt.Errorf("updating theme %s: %w", t.ID, err)
	}
	return checkAffected(res, "theme", t.ID)
}

// Delete removes a theme.
func (r *CustomThemeRepo) Delete(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM custom_themes WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting theme %s: %w", id, err)
	}
	return checkAffected(res, "theme", id)
}

func encodeVars(vars map[string]string) (string, error) {
	if vars == nil {
		return "{}", nil
	}
	raw, err := json.Marshal(vars)
	if err != nil {
		return "", fmt.Errorf("encoding theme variables: %w", err)
	}
	return string(raw), nil
}

func scanCustomTheme(row rowScanner) (*CustomTheme, error) {
	var (
		t         CustomTheme
		vars      string
		createdAt string
		updatedAt string
	)
	err := row.Scan(&t.ID, &t.UserID, &t.Name, &t.Base, &vars, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scanning theme: %w", err)
	}

	if err := json.Unmarshal([]byte(vars), &t.Vars); err != nil {
		return nil, fmt.Errorf("decoding theme variables: %w", err)
	}
	if t.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, fmt.Errorf("parsing theme created_at: %w", err)
	}
	if t.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, fmt.Errorf("parsing theme updated_at: %w", err)
	}
	return &t, nil
}

// AuditRepo appends to the audit log.
type AuditRepo struct{ db *sql.DB }

// Append records one mutating action.
func (r *AuditRepo) Append(ctx context.Context, e *AuditEntry) error {
	if e.ID == "" {
		e.ID = NewID()
	}
	e.CreatedAt = time.Now().UTC()

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO audit_log (id, user_id, action, target, ip, details, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.UserID, e.Action, e.Target, e.IP, e.Details, formatTime(e.CreatedAt))
	if err != nil {
		return fmt.Errorf("appending audit entry %s: %w", e.Action, err)
	}
	return nil
}

// List returns audit entries, newest first.
func (r *AuditRepo) List(ctx context.Context, userID string, limit int) ([]*AuditEntry, error) {
	if limit <= 0 || limit > 200 {
		limit = 200
	}

	query := `SELECT id, user_id, action, target, ip, details, created_at FROM audit_log`
	args := []any{}
	if userID != "" {
		query += " WHERE user_id = ?"
		args = append(args, userID)
	}
	query += " ORDER BY created_at DESC, id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing audit entries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var entries []*AuditEntry
	for rows.Next() {
		var (
			e         AuditEntry
			createdAt string
		)
		if err := rows.Scan(&e.ID, &e.UserID, &e.Action, &e.Target, &e.IP, &e.Details, &createdAt); err != nil {
			return nil, fmt.Errorf("scanning audit entry: %w", err)
		}
		if e.CreatedAt, err = parseTime(createdAt); err != nil {
			return nil, fmt.Errorf("parsing audit created_at: %w", err)
		}
		entries = append(entries, &e)
	}
	return entries, rows.Err()
}
