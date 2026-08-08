package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Webhook is an HTTP endpoint that receives events.
type Webhook struct {
	ID      string
	UserID  string
	URL     string
	Secret  string
	Events  []string
	Enabled bool

	LastStatus    *int
	LastError     string
	LastAttemptAt *time.Time
	LastSuccessAt *time.Time
	FailureCount  int

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Wants reports whether a hook subscribes to an event type. A hook with no
// event list receives everything, which is the useful default for someone
// wiring up a bot.
func (w *Webhook) Wants(eventType string) bool {
	if len(w.Events) == 0 {
		return true
	}
	for _, e := range w.Events {
		if e == eventType {
			return true
		}
	}
	return false
}

// WebhookRepo stores webhooks.
type WebhookRepo struct{ db *sql.DB }

const webhookColumns = `id, user_id, url, secret, events, enabled,
	last_status, last_error, last_attempt_at, last_success_at, failure_count,
	created_at, updated_at`

// Create inserts a webhook.
func (r *WebhookRepo) Create(ctx context.Context, w *Webhook) error {
	if w.ID == "" {
		w.ID = NewID()
	}
	now := time.Now().UTC()
	w.CreatedAt, w.UpdatedAt = now, now

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO webhooks (`+webhookColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		w.ID, w.UserID, w.URL, w.Secret, encodeScopes(w.Events), w.Enabled,
		nil, "", nil, nil, 0,
		formatTime(w.CreatedAt), formatTime(w.UpdatedAt))
	if err != nil {
		return fmt.Errorf("creating a webhook for user %s: %w", w.UserID, err)
	}
	return nil
}

// GetByID returns one webhook.
func (r *WebhookRepo) GetByID(ctx context.Context, id string) (*Webhook, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+webhookColumns+` FROM webhooks WHERE id = ?`, id)
	return scanWebhook(row)
}

// ListByUser returns a user's webhooks.
func (r *WebhookRepo) ListByUser(ctx context.Context, userID string) ([]*Webhook, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+webhookColumns+` FROM webhooks WHERE user_id = ? ORDER BY id`, userID)
	if err != nil {
		return nil, fmt.Errorf("listing webhooks for user %s: %w", userID, err)
	}
	defer func() { _ = rows.Close() }()

	return collectWebhooks(rows)
}

// ListEnabled returns every hook that is switched on, for the dispatcher.
func (r *WebhookRepo) ListEnabled(ctx context.Context) ([]*Webhook, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+webhookColumns+` FROM webhooks WHERE enabled = 1`)
	if err != nil {
		return nil, fmt.Errorf("listing enabled webhooks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return collectWebhooks(rows)
}

// Update writes a webhook's mutable fields.
func (r *WebhookRepo) Update(ctx context.Context, w *Webhook) error {
	w.UpdatedAt = time.Now().UTC()

	res, err := r.db.ExecContext(ctx, `
		UPDATE webhooks SET url = ?, secret = ?, events = ?, enabled = ?, updated_at = ?
		WHERE id = ?`,
		w.URL, w.Secret, encodeScopes(w.Events), w.Enabled, formatTime(w.UpdatedAt), w.ID)
	if err != nil {
		return fmt.Errorf("updating webhook %s: %w", w.ID, err)
	}
	return checkAffected(res, "webhook", w.ID)
}

// Delete removes a webhook.
func (r *WebhookRepo) Delete(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM webhooks WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting webhook %s: %w", id, err)
	}
	return checkAffected(res, "webhook", id)
}

// RecordDelivery stores the outcome of a delivery attempt.
//
// Kept so an operator can see a hook that has been failing for a week, rather
// than discovering it when they wonder why their bot went quiet.
func (r *WebhookRepo) RecordDelivery(ctx context.Context, id string, status int, deliveryErr string, at time.Time) error {
	var (
		statusValue any = status
		successAt   any
		failure     = "failure_count = failure_count + 1"
	)
	if status == 0 {
		statusValue = nil
	}
	if deliveryErr == "" {
		successAt = formatTime(at)
		failure = "failure_count = 0"
	}

	_, err := r.db.ExecContext(ctx, `
		UPDATE webhooks SET
			last_status = ?, last_error = ?, last_attempt_at = ?,
			last_success_at = COALESCE(?, last_success_at), `+failure+`
		WHERE id = ?`,
		statusValue, deliveryErr, formatTime(at), successAt, id)
	if err != nil {
		return fmt.Errorf("recording a delivery for webhook %s: %w", id, err)
	}
	return nil
}

func collectWebhooks(rows *sql.Rows) ([]*Webhook, error) {
	var out []*Webhook
	for rows.Next() {
		w, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

func scanWebhook(row rowScanner) (*Webhook, error) {
	var (
		w          Webhook
		events     string
		enabled    int
		lastStatus sql.NullInt64
		attemptAt  sql.NullString
		successAt  sql.NullString
		createdAt  string
		updatedAt  string
	)
	err := row.Scan(&w.ID, &w.UserID, &w.URL, &w.Secret, &events, &enabled,
		&lastStatus, &w.LastError, &attemptAt, &successAt, &w.FailureCount,
		&createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scanning webhook: %w", err)
	}

	w.Events = decodeScopes(events)
	w.Enabled = enabled != 0
	if lastStatus.Valid {
		status := int(lastStatus.Int64)
		w.LastStatus = &status
	}
	if w.LastAttemptAt, err = scanNullableTime(attemptAt); err != nil {
		return nil, fmt.Errorf("parsing last_attempt_at: %w", err)
	}
	if w.LastSuccessAt, err = scanNullableTime(successAt); err != nil {
		return nil, fmt.Errorf("parsing last_success_at: %w", err)
	}
	if w.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, fmt.Errorf("parsing the webhook created_at: %w", err)
	}
	if w.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, fmt.Errorf("parsing the webhook updated_at: %w", err)
	}
	return &w, nil
}
