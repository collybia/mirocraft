package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ServerRepo stores managed servers.
type ServerRepo struct{ db *sql.DB }

const serverColumns = `id, owner_id, name, core, version, kind, status, ram_mb, port,
	java_args, dir, jar_name, auto_start, auto_restart, eula_accepted, proxy_id,
	created_at, updated_at`

// ServerFilter narrows a listing. Empty fields are ignored.
type ServerFilter struct {
	OwnerID string
	Status  string
	Core    string
	Limit   int
}

// Create inserts a server.
func (r *ServerRepo) Create(ctx context.Context, s *Server) error {
	if s.ID == "" {
		s.ID = NewID()
	}
	if s.Kind == "" {
		s.Kind = KindServer
	}
	if s.Status == "" {
		s.Status = "stopped"
	}
	now := time.Now().UTC()
	s.CreatedAt, s.UpdatedAt = now, now

	_, err := r.db.ExecContext(ctx, `
		INSERT INTO servers (`+serverColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.OwnerID, s.Name, s.Core, s.Version, s.Kind, s.Status, s.RAMMb, s.Port,
		s.JavaArgs, s.Dir, s.JarName, s.AutoStart, s.AutoRestart, s.EULAAccepted,
		nullableString(s.ProxyID),
		formatTime(s.CreatedAt), formatTime(s.UpdatedAt))
	if err != nil {
		if isUniqueViolation(err, "idx_servers_port", "servers.port") {
			return ErrPortInUse
		}
		return fmt.Errorf("creating server %s: %w", s.Name, err)
	}
	return nil
}

// GetByID returns a server by id.
func (r *ServerRepo) GetByID(ctx context.Context, id string) (*Server, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+serverColumns+` FROM servers WHERE id = ?`, id)
	return scanServer(row)
}

// List returns servers matching the filter, oldest first.
func (r *ServerRepo) List(ctx context.Context, f ServerFilter) ([]*Server, error) {
	query := `SELECT ` + serverColumns + ` FROM servers`
	var (
		where []string
		args  []any
	)
	if f.OwnerID != "" {
		where = append(where, "owner_id = ?")
		args = append(args, f.OwnerID)
	}
	if f.Status != "" {
		where = append(where, "status = ?")
		args = append(args, f.Status)
	}
	if f.Core != "" {
		where = append(where, "core = ?")
		args = append(args, f.Core)
	}
	if len(where) > 0 {
		// The joined fragments are literals from the block above; every value
		// they compare against is bound as a parameter.
		query += " WHERE " + strings.Join(where, " AND ") // #nosec G202 -- fixed fragments, bound values
	}

	limit := f.Limit
	if limit <= 0 || limit > 200 {
		limit = 200
	}
	query += " ORDER BY id LIMIT ?"
	args = append(args, limit)

	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing servers: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var servers []*Server
	for rows.Next() {
		s, err := scanServer(rows)
		if err != nil {
			return nil, err
		}
		servers = append(servers, s)
	}
	return servers, rows.Err()
}

// CountByOwner reports how many servers a user owns, for limit enforcement.
func (r *ServerRepo) CountByOwner(ctx context.Context, ownerID string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM servers WHERE owner_id = ?`, ownerID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("counting servers for user %s: %w", ownerID, err)
	}
	return n, nil
}

// UsedRAMByOwner sums the RAM allocated to a user's servers, for limit checks.
func (r *ServerRepo) UsedRAMByOwner(ctx context.Context, ownerID string) (int, error) {
	var total sql.NullInt64
	err := r.db.QueryRowContext(ctx,
		`SELECT sum(ram_mb) FROM servers WHERE owner_id = ?`, ownerID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("summing ram for user %s: %w", ownerID, err)
	}
	return int(total.Int64), nil
}

// Update writes every mutable field of s.
func (r *ServerRepo) Update(ctx context.Context, s *Server) error {
	s.UpdatedAt = time.Now().UTC()

	res, err := r.db.ExecContext(ctx, `
		UPDATE servers SET
			name = ?, core = ?, version = ?, kind = ?, status = ?, ram_mb = ?, port = ?,
			java_args = ?, dir = ?, jar_name = ?, auto_start = ?, auto_restart = ?,
			eula_accepted = ?, proxy_id = ?, updated_at = ?
		WHERE id = ?`,
		s.Name, s.Core, s.Version, s.Kind, s.Status, s.RAMMb, s.Port,
		s.JavaArgs, s.Dir, s.JarName, s.AutoStart, s.AutoRestart, s.EULAAccepted,
		nullableString(s.ProxyID), formatTime(s.UpdatedAt), s.ID)
	if err != nil {
		if isUniqueViolation(err, "idx_servers_port", "servers.port") {
			return ErrPortInUse
		}
		return fmt.Errorf("updating server %s: %w", s.ID, err)
	}
	return checkAffected(res, "server", s.ID)
}

// UpdateStatus records a lifecycle change without touching anything else.
func (r *ServerRepo) UpdateStatus(ctx context.Context, id, status string) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE servers SET status = ?, updated_at = ? WHERE id = ?`,
		status, formatTime(time.Now()), id)
	if err != nil {
		return fmt.Errorf("updating status of server %s: %w", id, err)
	}
	return checkAffected(res, "server", id)
}

// Delete removes a server. Backups cascade.
func (r *ServerRepo) Delete(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `DELETE FROM servers WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting server %s: %w", id, err)
	}
	return checkAffected(res, "server", id)
}

// PortInUse reports whether a port is already assigned to any server.
func (r *ServerRepo) PortInUse(ctx context.Context, port int) (bool, error) {
	return r.PortTaken(ctx, port, "")
}

// PortTaken reports whether a port is assigned to a server other than
// exceptID.
//
// The exclusion is what makes it usable when editing: without it, a server
// keeping its own port would be told the port is taken — by itself.
func (r *ServerRepo) PortTaken(ctx context.Context, port int, exceptID string) (bool, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT count(*) FROM servers WHERE port = ? AND id != ?`, port, exceptID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("checking port %d: %w", port, err)
	}
	return n > 0, nil
}

// AllocatePort returns the lowest free port in [from, to].
//
// It only knows about ports this panel has assigned; whether the host itself
// has something else bound is checked when the server actually starts.
func (r *ServerRepo) AllocatePort(ctx context.Context, from, to int) (int, error) {
	if from <= 0 || to < from {
		return 0, fmt.Errorf("invalid port range %d-%d", from, to)
	}

	rows, err := r.db.QueryContext(ctx,
		`SELECT port FROM servers WHERE port BETWEEN ? AND ? ORDER BY port`, from, to)
	if err != nil {
		return 0, fmt.Errorf("reading assigned ports: %w", err)
	}
	defer func() { _ = rows.Close() }()

	taken := make(map[int]struct{})
	for rows.Next() {
		var port int
		if err := rows.Scan(&port); err != nil {
			return 0, fmt.Errorf("scanning port: %w", err)
		}
		taken[port] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	for port := from; port <= to; port++ {
		if _, used := taken[port]; !used {
			return port, nil
		}
	}
	return 0, fmt.Errorf("no free port in range %d-%d", from, to)
}

func scanServer(row rowScanner) (*Server, error) {
	var (
		s            Server
		autoStart    int
		autoRestart  int
		eulaAccepted int
		proxyID      sql.NullString
		createdAt    string
		updatedAt    string
	)
	err := row.Scan(&s.ID, &s.OwnerID, &s.Name, &s.Core, &s.Version, &s.Kind, &s.Status,
		&s.RAMMb, &s.Port, &s.JavaArgs, &s.Dir, &s.JarName,
		&autoStart, &autoRestart, &eulaAccepted, &proxyID, &createdAt, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scanning server: %w", err)
	}

	s.ProxyID = proxyID.String
	s.AutoStart = autoStart != 0
	s.AutoRestart = autoRestart != 0
	s.EULAAccepted = eulaAccepted != 0
	if s.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, fmt.Errorf("parsing server created_at: %w", err)
	}
	if s.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return nil, fmt.Errorf("parsing server updated_at: %w", err)
	}
	return &s, nil
}

// Backends returns the servers sitting behind a proxy, by name.
//
// Ordered by name because that is the order they will be written into the
// proxy's configuration, and a configuration that reshuffles itself on every
// start is one an operator cannot diff.
func (r *ServerRepo) Backends(ctx context.Context, proxyID string) ([]*Server, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT `+serverColumns+` FROM servers WHERE proxy_id = ? ORDER BY name`, proxyID)
	if err != nil {
		return nil, fmt.Errorf("listing the servers behind proxy %s: %w", proxyID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []*Server
	for rows.Next() {
		server, err := scanServer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, server)
	}
	return out, rows.Err()
}
