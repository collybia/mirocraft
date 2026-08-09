package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The states a configured bot can be in, as the panel shows them.
const (
	BotStatusOff        = "off"
	BotStatusConnecting = "connecting"
	BotStatusConnected  = "connected"
	BotStatusFailed     = "failed"
)

// BotSettings is one platform's configuration.
//
// Token is the secret the daemon presents to the platform. It is the one
// credential here that cannot be hashed, because it is presented rather than
// verified; every reader of this struct is responsible for not letting it out
// — in particular, it has no place in a log line or an API response.
type BotSettings struct {
	Provider string
	Token    string
	Enabled  bool

	LastStatus string
	LastError  string
	Account    string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Configured reports whether a token has been saved.
func (b *BotSettings) Configured() bool { return b != nil && b.Token != "" }

// TokenHint returns the last four characters of the token, so an operator can
// recognise which of their bots this is without the panel handing the secret
// back. Empty when nothing is configured.
func (b *BotSettings) TokenHint() string {
	if !b.Configured() {
		return ""
	}
	if len(b.Token) <= 4 {
		// Too short to be a real token; showing any of it would be showing
		// most of it.
		return "…"
	}
	return "…" + b.Token[len(b.Token)-4:]
}

// BotRepo stores bot configuration.
type BotRepo struct{ db *sql.DB }

const botColumns = `provider, token, enabled, last_status, last_error, account, created_at, updated_at` // #nosec G101 -- a column list

// Get returns one platform's settings, or ErrNotFound when it has never been
// configured.
func (r *BotRepo) Get(ctx context.Context, provider string) (*BotSettings, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+botColumns+` FROM bot_settings WHERE provider = ?`, provider)
	return scanBot(row)
}

// List returns every configured platform.
func (r *BotRepo) List(ctx context.Context) ([]*BotSettings, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+botColumns+` FROM bot_settings ORDER BY provider`)
	if err != nil {
		return nil, fmt.Errorf("listing bot settings: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*BotSettings
	for rows.Next() {
		settings, err := scanBot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, settings)
	}
	return out, rows.Err()
}

// Save writes a platform's token and switch, leaving the connection status
// alone: that belongs to whoever is running the session, not to whoever is
// editing the settings.
func (r *BotRepo) Save(ctx context.Context, provider, token string, enabled bool) error {
	now := time.Now().UTC()

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO bot_settings (provider, token, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (provider) DO UPDATE SET
			token = excluded.token,
			enabled = excluded.enabled,
			updated_at = excluded.updated_at`,
		provider, token, enabled, formatTime(now), formatTime(now))
	if err != nil {
		return fmt.Errorf("saving %s settings: %w", provider, err)
	}
	return nil
}

// SetStatus records what the last connection attempt did.
func (r *BotRepo) SetStatus(ctx context.Context, provider, status, failure, account string) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE bot_settings
		SET last_status = ?, last_error = ?, account = ?, updated_at = ?
		WHERE provider = ?`,
		status, failure, account, formatTime(time.Now().UTC()), provider)
	if err != nil {
		return fmt.Errorf("recording %s status: %w", provider, err)
	}
	return nil
}

// Delete forgets a platform entirely, token included.
func (r *BotRepo) Delete(ctx context.Context, provider string) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM bot_settings WHERE provider = ?`, provider)
	if err != nil {
		return fmt.Errorf("deleting %s settings: %w", provider, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("deleting %s settings: %w", provider, err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

func scanBot(row scanner) (*BotSettings, error) {
	var b BotSettings
	var created, updated string

	err := row.Scan(&b.Provider, &b.Token, &b.Enabled,
		&b.LastStatus, &b.LastError, &b.Account, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("reading bot settings: %w", err)
	}

	if b.CreatedAt, err = parseTime(created); err != nil {
		return nil, fmt.Errorf("reading bot settings: %w", err)
	}
	if b.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, fmt.Errorf("reading bot settings: %w", err)
	}
	return &b, nil
}
