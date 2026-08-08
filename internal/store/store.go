package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"

	// Pure-Go SQLite driver: no CGO, so cross-compiling to linux/arm64 and
	// windows/amd64 keeps working.
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// timeFormat is the storage format for timestamps: RFC 3339 in UTC, the same
// format the API emits.
const timeFormat = time.RFC3339Nano

// Store is the database handle plus the repositories built on it.
type Store struct {
	db *sql.DB

	Users        *UserRepo
	Tokens       *TokenRepo
	Servers      *ServerRepo
	Backups      *BackupRepo
	Webhooks     *WebhookRepo
	CustomThemes *CustomThemeRepo
	Audit        *AuditRepo
}

// Open opens (or creates) the database at path and applies pending migrations.
// The special path ":memory:" gives an in-memory database, which is what the
// tests use.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := path
	if path != ":memory:" {
		// Busy timeout keeps concurrent writers from failing outright, and
		// WAL is set below once the connection is up.
		dsn = path + "?_pragma=busy_timeout(5000)"
	}

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening database %s: %w", path, err)
	}

	// SQLite takes a write lock per database, so extra writer connections only
	// produce lock contention. One connection also keeps an in-memory database
	// from appearing empty to the next caller.
	db.SetMaxOpenConns(1)

	pragmas := []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
	}
	for _, pragma := range pragmas {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("applying %q: %w", pragma, err)
		}
	}

	s := &Store{db: db}
	s.Users = &UserRepo{db: db}
	s.Tokens = &TokenRepo{db: db}
	s.Servers = &ServerRepo{db: db}
	s.Backups = &BackupRepo{db: db}
	s.Webhooks = &WebhookRepo{db: db}
	s.CustomThemes = &CustomThemeRepo{db: db}
	s.Audit = &AuditRepo{db: db}

	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// DB exposes the underlying handle for packages that need raw access.
func (s *Store) DB() *sql.DB { return s.db }

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// migrate applies every embedded migration that has not run yet, in filename
// order, each inside its own transaction.
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name       TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL
		)`); err != nil {
		return fmt.Errorf("creating migration table: %w", err)
	}

	applied, err := s.appliedMigrations(ctx)
	if err != nil {
		return err
	}

	names, err := migrationNames()
	if err != nil {
		return err
	}

	for _, name := range names {
		if _, done := applied[name]; done {
			continue
		}

		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("reading migration %s: %w", name, err)
		}

		if err := s.applyMigration(ctx, name, string(body)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) applyMigration(ctx context.Context, name, body string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("starting migration %s: %w", name, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, body); err != nil {
		return fmt.Errorf("applying migration %s: %w", name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`,
		name, formatTime(time.Now())); err != nil {
		return fmt.Errorf("recording migration %s: %w", name, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing migration %s: %w", name, err)
	}
	return nil
}

func (s *Store) appliedMigrations(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("reading applied migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	applied := make(map[string]struct{})
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scanning applied migration: %w", err)
		}
		applied[name] = struct{}{}
	}
	return applied, rows.Err()
}

// migrationNames lists embedded migrations in lexicographic order, which is
// why they are numbered.
func migrationNames() ([]string, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("listing migrations: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names, nil
}

func formatTime(t time.Time) string {
	return t.UTC().Format(timeFormat)
}

func parseTime(raw string) (time.Time, error) {
	return time.Parse(timeFormat, raw)
}

// nullableTime maps a *time.Time onto a nullable column value.
func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

// scanNullableTime reads a nullable timestamp column.
func scanNullableTime(raw sql.NullString) (*time.Time, error) {
	if !raw.Valid || raw.String == "" {
		return nil, nil
	}
	t, err := parseTime(raw.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// isUniqueViolation reports whether err is a SQLite uniqueness failure naming
// any of the given markers.
//
// The driver reports these as a plain message rather than a typed error, so
// the constraint is matched textually. Which name appears depends on the
// index: a plain column index reports "servers.port", while an expression
// index like lower(email) reports "index 'idx_users_email'". Callers pass
// every form the constraint can surface as.
func isUniqueViolation(err error, markers ...string) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if !strings.Contains(msg, "UNIQUE constraint failed") {
		return false
	}
	for _, marker := range markers {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}
