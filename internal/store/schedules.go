package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Action types a schedule may chain.
const (
	ActionCommand = "command"
	ActionPower   = "power"
	ActionBackup  = "backup"
	ActionWait    = "wait"
)

// Outcomes recorded after a run.
const (
	ScheduleRunOK     = "ok"
	ScheduleRunFailed = "failed"
)

// Action is one step of a schedule.
//
// The payload is loose on purpose: each type reads different fields, and a
// struct with every field of every type would be mostly empty whichever type
// it holds. The API validates the fields a type actually needs.
type Action struct {
	Type    string         `json:"type"`
	Payload map[string]any `json:"payload,omitempty"`
}

// String returns the payload field as a string, empty when it is missing or
// not a string.
func (a Action) String(key string) string {
	s, _ := a.Payload[key].(string)
	return s
}

// Int returns the payload field as an int. JSON numbers decode to float64, so
// both forms are accepted.
func (a Action) Int(key string) (int, bool) {
	switch v := a.Payload[key].(type) {
	case float64:
		return int(v), true
	case int:
		return v, true
	}
	return 0, false
}

// Schedule is a named chain of actions on a cron.
type Schedule struct {
	ID       string
	ServerID string
	Name     string
	Cron     string
	Actions  []Action
	Enabled  bool

	LastRunAt  *time.Time
	LastStatus string
	LastError  string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// ScheduleRepo stores schedules.
type ScheduleRepo struct{ db *sql.DB }

const scheduleRowColumns = `id, server_id, name, cron, actions, enabled,
	last_run_at, last_status, last_error, created_at, updated_at`

// Create inserts a schedule.
func (r *ScheduleRepo) Create(ctx context.Context, s *Schedule) error {
	if s.ID == "" {
		s.ID = NewID()
	}
	now := time.Now().UTC()
	s.CreatedAt, s.UpdatedAt = now, now

	actions, err := encodeActions(s.Actions)
	if err != nil {
		return err
	}

	_, err = r.db.ExecContext(ctx, `
		INSERT INTO schedules (`+scheduleRowColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.ServerID, s.Name, s.Cron, actions, s.Enabled,
		nullableTime(s.LastRunAt), s.LastStatus, s.LastError,
		formatTime(s.CreatedAt), formatTime(s.UpdatedAt))
	if err != nil {
		return fmt.Errorf("creating a schedule for server %s: %w", s.ServerID, err)
	}
	return nil
}

// GetByID returns one schedule.
func (r *ScheduleRepo) GetByID(ctx context.Context, id string) (*Schedule, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+scheduleRowColumns+` FROM schedules WHERE id = ?`, id)
	return scanScheduleRow(row)
}

// ListByServer returns a server's schedules, oldest first.
func (r *ScheduleRepo) ListByServer(ctx context.Context, serverID string) ([]*Schedule, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+scheduleRowColumns+` FROM schedules WHERE server_id = ? ORDER BY id`, serverID)
	if err != nil {
		return nil, fmt.Errorf("listing schedules for server %s: %w", serverID, err)
	}
	defer func() { _ = rows.Close() }()

	return collectSchedules(rows)
}

// ListEnabled returns every schedule that is switched on, for the runner.
func (r *ScheduleRepo) ListEnabled(ctx context.Context) ([]*Schedule, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+scheduleRowColumns+` FROM schedules WHERE enabled = 1`)
	if err != nil {
		return nil, fmt.Errorf("listing enabled schedules: %w", err)
	}
	defer func() { _ = rows.Close() }()

	return collectSchedules(rows)
}

// Update writes a schedule's mutable fields.
func (r *ScheduleRepo) Update(ctx context.Context, s *Schedule) error {
	s.UpdatedAt = time.Now().UTC()

	actions, err := encodeActions(s.Actions)
	if err != nil {
		return err
	}

	res, err := r.db.ExecContext(ctx, `
		UPDATE schedules SET name = ?, cron = ?, actions = ?, enabled = ?, updated_at = ?
		WHERE id = ?`,
		s.Name, s.Cron, actions, s.Enabled, formatTime(s.UpdatedAt), s.ID)
	if err != nil {
		return fmt.Errorf("updating schedule %s: %w", s.ID, err)
	}
	return checkAffected(res, "schedule", s.ID)
}

// Delete removes a schedule.
func (r *ScheduleRepo) Delete(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM schedules WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting schedule %s: %w", id, err)
	}
	return checkAffected(res, "schedule", id)
}

// MarkRun records that a schedule fired, before its chain runs.
//
// Separate from RecordOutcome because the two happen at different times: a
// chain that waits a minute between actions must not be started again on the
// next tick just because it has not finished yet.
func (r *ScheduleRepo) MarkRun(ctx context.Context, id string, at time.Time) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE schedules SET last_run_at = ?, last_status = '', last_error = '' WHERE id = ?`,
		formatTime(at), id)
	if err != nil {
		return fmt.Errorf("recording the run of schedule %s: %w", id, err)
	}
	return nil
}

// RecordOutcome stores how a run ended.
func (r *ScheduleRepo) RecordOutcome(ctx context.Context, id, status, runErr string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE schedules SET last_status = ?, last_error = ? WHERE id = ?`,
		status, runErr, id)
	if err != nil {
		return fmt.Errorf("recording the outcome of schedule %s: %w", id, err)
	}
	return nil
}

// CountByServer reports how many schedules a server has, for the limit.
func (r *ScheduleRepo) CountByServer(ctx context.Context, serverID string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schedules WHERE server_id = ?`, serverID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting schedules for server %s: %w", serverID, err)
	}
	return n, nil
}

func encodeActions(actions []Action) (string, error) {
	if actions == nil {
		actions = []Action{}
	}
	raw, err := json.Marshal(actions)
	if err != nil {
		return "", fmt.Errorf("encoding the action chain: %w", err)
	}
	return string(raw), nil
}

func collectSchedules(rows *sql.Rows) ([]*Schedule, error) {
	var out []*Schedule
	for rows.Next() {
		s, err := scanScheduleRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func scanScheduleRow(row rowScanner) (*Schedule, error) {
	var (
		s         Schedule
		actions   string
		enabled   int
		lastRun   sql.NullString
		createdAt string
		updatedAt string
	)
	err := row.Scan(&s.ID, &s.ServerID, &s.Name, &s.Cron, &actions, &enabled,
		&lastRun, &s.LastStatus, &s.LastError, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scanning schedule: %w", err)
	}

	if err := json.Unmarshal([]byte(actions), &s.Actions); err != nil {
		return nil, fmt.Errorf("decoding the action chain of schedule %s: %w", s.ID, err)
	}
	s.Enabled = enabled != 0
	if s.LastRunAt, err = scanNullableTime(lastRun); err != nil {
		return nil, fmt.Errorf("parsing the schedule last_run_at: %w", err)
	}
	if s.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, fmt.Errorf("parsing the schedule created_at: %w", err)
	}
	if s.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, fmt.Errorf("parsing the schedule updated_at: %w", err)
	}
	return &s, nil
}
