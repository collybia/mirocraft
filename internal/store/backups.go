package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Backup states.
const (
	BackupPending = "pending"
	BackupRunning = "running"
	BackupDone    = "done"
	BackupFailed  = "failed"
)

// BackupSchedule is a server's automatic backup setting.
type BackupSchedule struct {
	ServerID  string
	Cron      string
	KeepLast  int
	Enabled   bool
	LastRunAt *time.Time
	CreatedAt time.Time
	UpdatedAt time.Time
}

// BackupRepo stores backup records and schedules.
type BackupRepo struct{ db *sql.DB }

const backupColumns = `id, server_id, note, state, size_bytes, path, created_at`

// Create inserts a backup record.
func (r *BackupRepo) Create(ctx context.Context, b *Backup) error {
	if b.ID == "" {
		b.ID = NewID()
	}
	if b.State == "" {
		b.State = BackupPending
	}
	b.CreatedAt = time.Now().UTC()

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO backups (`+backupColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		b.ID, b.ServerID, b.Note, b.State, b.SizeBytes, b.Path, formatTime(b.CreatedAt))
	if err != nil {
		return fmt.Errorf("creating a backup record for server %s: %w", b.ServerID, err)
	}
	return nil
}

// GetByID returns one backup.
func (r *BackupRepo) GetByID(ctx context.Context, id string) (*Backup, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+backupColumns+` FROM backups WHERE id = ?`, id)
	return scanBackup(row)
}

// ListByServer returns a server's backups, newest first.
func (r *BackupRepo) ListByServer(ctx context.Context, serverID string) ([]*Backup, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+backupColumns+` FROM backups WHERE server_id = ?
		 ORDER BY created_at DESC, id DESC`, serverID)
	if err != nil {
		return nil, fmt.Errorf("listing backups for server %s: %w", serverID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Backup
	for rows.Next() {
		b, err := scanBackup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Finish records the outcome of a backup run.
func (r *BackupRepo) Finish(ctx context.Context, id, state, path string, size int64) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE backups SET state = ?, path = ?, size_bytes = ? WHERE id = ?`,
		state, path, size, id)
	if err != nil {
		return fmt.Errorf("finishing backup %s: %w", id, err)
	}
	return checkAffected(res, "backup", id)
}

// SetState moves a backup to another state without touching its artifact.
func (r *BackupRepo) SetState(ctx context.Context, id, state string) error {
	res, err := r.db.ExecContext(ctx, `UPDATE backups SET state = ? WHERE id = ?`, state, id)
	if err != nil {
		return fmt.Errorf("updating backup %s: %w", id, err)
	}
	return checkAffected(res, "backup", id)
}

// Delete removes a backup record.
func (r *BackupRepo) Delete(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM backups WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting backup %s: %w", id, err)
	}
	return checkAffected(res, "backup", id)
}

// RunningFor reports whether a backup is already in flight for a server.
//
// Two archivers writing the same directory at once produce two archives that
// each caught it mid-copy, so the second request is refused rather than
// queued behind the first.
func (r *BackupRepo) RunningFor(ctx context.Context, serverID string) (bool, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM backups WHERE server_id = ? AND state IN (?, ?)`,
		serverID, BackupPending, BackupRunning).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("checking running backups for server %s: %w", serverID, err)
	}
	return n > 0, nil
}

// Prune returns the backups beyond the newest keep, oldest first, so a caller
// can delete their artifacts before dropping the records.
func (r *BackupRepo) Prune(ctx context.Context, serverID string, keep int) ([]*Backup, error) {
	if keep < 0 {
		keep = 0
	}

	all, err := r.ListByServer(ctx, serverID)
	if err != nil {
		return nil, err
	}

	// Only completed backups count towards the limit: pruning one that is
	// still running would delete an archive out from under the writer.
	var done []*Backup
	for _, b := range all {
		if b.State == BackupDone {
			done = append(done, b)
		}
	}
	if len(done) <= keep {
		return nil, nil
	}
	return done[keep:], nil
}

func scanBackup(row rowScanner) (*Backup, error) {
	var (
		b         Backup
		createdAt string
	)
	err := row.Scan(&b.ID, &b.ServerID, &b.Note, &b.State, &b.SizeBytes, &b.Path, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scanning backup: %w", err)
	}

	if b.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, fmt.Errorf("parsing backup created_at: %w", err)
	}
	return &b, nil
}

// --- schedules ---

const scheduleColumns = `server_id, cron, keep_last, enabled, last_run_at, created_at, updated_at`

// GetSchedule returns a server's backup schedule.
func (r *BackupRepo) GetSchedule(ctx context.Context, serverID string) (*BackupSchedule, error) {
	row := r.db.QueryRowContext(ctx,
		`SELECT `+scheduleColumns+` FROM backup_schedules WHERE server_id = ?`, serverID)
	return scanSchedule(row)
}

// SetSchedule creates or replaces a server's schedule.
func (r *BackupRepo) SetSchedule(ctx context.Context, s *BackupSchedule) error {
	now := time.Now().UTC()
	s.UpdatedAt = now
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO backup_schedules (`+scheduleColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (server_id) DO UPDATE SET
			cron = excluded.cron,
			keep_last = excluded.keep_last,
			enabled = excluded.enabled,
			updated_at = excluded.updated_at`,
		s.ServerID, s.Cron, s.KeepLast, s.Enabled,
		nullableTime(s.LastRunAt), formatTime(s.CreatedAt), formatTime(s.UpdatedAt))
	if err != nil {
		return fmt.Errorf("saving the backup schedule for server %s: %w", s.ServerID, err)
	}
	return nil
}

// ListEnabledSchedules returns every schedule that is switched on.
func (r *BackupRepo) ListEnabledSchedules(ctx context.Context) ([]*BackupSchedule, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+scheduleColumns+` FROM backup_schedules WHERE enabled = 1`)
	if err != nil {
		return nil, fmt.Errorf("listing backup schedules: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []*BackupSchedule
	for rows.Next() {
		s, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// MarkScheduleRun records that a scheduled backup fired.
func (r *BackupRepo) MarkScheduleRun(ctx context.Context, serverID string, at time.Time) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE backup_schedules SET last_run_at = ? WHERE server_id = ?`,
		formatTime(at), serverID)
	if err != nil {
		return fmt.Errorf("recording the schedule run for server %s: %w", serverID, err)
	}
	return nil
}

func scanSchedule(row rowScanner) (*BackupSchedule, error) {
	var (
		s         BackupSchedule
		enabled   int
		lastRun   sql.NullString
		createdAt string
		updatedAt string
	)
	err := row.Scan(&s.ServerID, &s.Cron, &s.KeepLast, &enabled, &lastRun, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scanning the backup schedule: %w", err)
	}

	s.Enabled = enabled != 0
	if s.LastRunAt, err = scanNullableTime(lastRun); err != nil {
		return nil, fmt.Errorf("parsing last_run_at: %w", err)
	}
	if s.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, fmt.Errorf("parsing the schedule created_at: %w", err)
	}
	if s.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, fmt.Errorf("parsing the schedule updated_at: %w", err)
	}
	return &s, nil
}
