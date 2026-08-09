package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// TokenPrefix marks a Mirocraft API token, so a leaked string is recognisable
// in logs and secret scanners.
const TokenPrefix = "mcr_"

// TokenRepo stores API and session tokens. Only hashes are persisted.
type TokenRepo struct{ db *sql.DB }

// tokenColumns is the column list, not a secret; only hashes are ever stored.
const tokenColumns = `id, user_id, name, hash, scopes, kind, expires_at, last_used_at, created_at` // #nosec G101 -- a column list

// GenerateToken returns a new random token value and its storage hash. The
// value is shown to the user exactly once and never stored.
func GenerateToken() (value, hash string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", fmt.Errorf("generating token: %w", err)
	}
	value = TokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	return value, HashToken(value), nil
}

// HashToken returns the storage form of a token value.
func HashToken(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// Create inserts a token record. The caller supplies the hash from
// GenerateToken; the plaintext value never reaches this layer.
func (r *TokenRepo) Create(ctx context.Context, t *Token) error {
	if t.ID == "" {
		t.ID = NewID()
	}
	if t.Kind == "" {
		t.Kind = TokenKindAPI
	}
	t.CreatedAt = time.Now().UTC()

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO tokens (`+tokenColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.UserID, t.Name, t.Hash, encodeScopes(t.Scopes), t.Kind,
		nullableTime(t.ExpiresAt), nullableTime(t.LastUsedAt), formatTime(t.CreatedAt))
	if err != nil {
		return fmt.Errorf("creating token for user %s: %w", t.UserID, err)
	}
	return nil
}

// GetByHash looks a token up by its hash. Expiry is not checked here; callers
// decide what to do with an expired token.
func (r *TokenRepo) GetByHash(ctx context.Context, hash string) (*Token, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+tokenColumns+` FROM tokens WHERE hash = ?`, hash)
	return scanToken(row)
}

// GetByID returns a token record by id.
func (r *TokenRepo) GetByID(ctx context.Context, id string) (*Token, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+tokenColumns+` FROM tokens WHERE id = ?`, id)
	return scanToken(row)
}

// ListByUser returns a user's tokens, newest first. Values are never returned
// because they are not stored.
func (r *TokenRepo) ListByUser(ctx context.Context, userID string) ([]*Token, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+tokenColumns+` FROM tokens WHERE user_id = ? ORDER BY id DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("listing tokens for user %s: %w", userID, err)
	}
	defer func() { _ = rows.Close() }()

	var tokens []*Token
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

// TouchLastUsed records that a token was used. Failures are the caller's to
// ignore: it is bookkeeping, not authorization.
func (r *TokenRepo) TouchLastUsed(ctx context.Context, id string, at time.Time) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE tokens SET last_used_at = ? WHERE id = ?`, formatTime(at), id)
	if err != nil {
		return fmt.Errorf("touching token %s: %w", id, err)
	}
	return nil
}

// Delete revokes a token.
func (r *TokenRepo) Delete(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM tokens WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting token %s: %w", id, err)
	}
	if err := checkAffected(res, "token", id); err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrTokenNotFound
		}
		return err
	}
	return nil
}

// DeleteByHash revokes the token with the given hash, used on logout.
func (r *TokenRepo) DeleteByHash(ctx context.Context, hash string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM tokens WHERE hash = ?`, hash)
	if err != nil {
		return fmt.Errorf("deleting token by hash: %w", err)
	}
	if err := checkAffected(res, "token", "by hash"); err != nil {
		if errors.Is(err, ErrNotFound) {
			return ErrTokenNotFound
		}
		return err
	}
	return nil
}

// DeleteExpired removes tokens whose expiry has passed and reports how many
// were removed.
func (r *TokenRepo) DeleteExpired(ctx context.Context, now time.Time) (int64, error) {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM tokens WHERE expires_at IS NOT NULL AND expires_at <= ?`,
		formatTime(now))
	if err != nil {
		return 0, fmt.Errorf("deleting expired tokens: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("counting deleted tokens: %w", err)
	}
	return n, nil
}

func scanToken(row rowScanner) (*Token, error) {
	var (
		t          Token
		scopes     string
		expiresAt  sql.NullString
		lastUsedAt sql.NullString
		createdAt  string
	)
	err := row.Scan(&t.ID, &t.UserID, &t.Name, &t.Hash, &scopes, &t.Kind,
		&expiresAt, &lastUsedAt, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTokenNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scanning token: %w", err)
	}

	t.Scopes = decodeScopes(scopes)
	if t.ExpiresAt, err = scanNullableTime(expiresAt); err != nil {
		return nil, fmt.Errorf("parsing token expires_at: %w", err)
	}
	if t.LastUsedAt, err = scanNullableTime(lastUsedAt); err != nil {
		return nil, fmt.Errorf("parsing token last_used_at: %w", err)
	}
	if t.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, fmt.Errorf("parsing token created_at: %w", err)
	}
	return &t, nil
}
