package api

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/collybia/mirocraft/internal/daemon"
	"github.com/collybia/mirocraft/internal/mcping"
	"github.com/collybia/mirocraft/internal/runner"
	"github.com/collybia/mirocraft/internal/store"
)

// Server limits and defaults.
const (
	MinRAMMb     = 512
	MaxRAMMb     = 512 * 1024
	MinPort      = 1024
	MaxPort      = 65535
	MaxServerLen = 64
)

// Power actions.
const (
	ActionStart   = "start"
	ActionStop    = "stop"
	ActionRestart = "restart"
	ActionKill    = "kill"
)

// serverNamePattern keeps names safe to use as directory names, which is what
// they become on disk.
// A name must start and end with an alphanumeric so it cannot be pure
// punctuation or carry leading and trailing spaces, but a single character is
// a perfectly good name — hence the alternation rather than a bare
// start-middle-end pattern, which silently imposed a two-character minimum.
var serverNamePattern = regexp.MustCompile(`^[a-zA-Z0-9]$|^[a-zA-Z0-9][a-zA-Z0-9 _-]{0,62}[a-zA-Z0-9]$`)

// Pinger asks a running server for its player counts.
//
// It is a field rather than a direct call to mcping so that tests are not at
// the mercy of whatever happens to be listening on the host: a test asserting
// that a fake server reports no players failed because a real Minecraft
// server was running on the same port on the developer's machine.
type Pinger func(ctx context.Context, host string, port int) (*mcping.Status, error)

// Provisioner puts a server's core jar and Java runtime in place before it
// starts. Nil means the daemon runs without one, in which case a start uses
// whatever the server record already points at.
type Provisioner interface {
	Prepare(ctx context.Context, srv *store.Server, dir string) (*daemon.Launch, error)
}

// Lifecycle is the slice of the runner the server endpoints need beyond the
// console.
type Lifecycle interface {
	Start(ctx context.Context, srv *runner.Server) error
	Stop(ctx context.Context, id string, timeout time.Duration) error
	Kill(ctx context.Context, id string) error
	Status(ctx context.Context, id string) (runner.Status, error)
	Stats(ctx context.Context, id string) (runner.Stats, error)
}

// --- wire types ---

type createServerRequest struct {
	Name         string `json:"name"`
	Core         string `json:"core"`
	Version      string `json:"version"`
	Kind         string `json:"kind"`
	RAMMb        int    `json:"ram_mb"`
	Port         int    `json:"port"`
	JavaArgs     string `json:"java_args"`
	EULAAccepted bool   `json:"eula_accepted"`
	AutoStart    bool   `json:"auto_start"`
	AutoRestart  bool   `json:"auto_restart"`
}

type patchServerRequest struct {
	Name        *string `json:"name"`
	RAMMb       *int    `json:"ram_mb"`
	JavaArgs    *string `json:"java_args"`
	AutoStart   *bool   `json:"auto_start"`
	AutoRestart *bool   `json:"auto_restart"`
}

type powerRequest struct {
	Action         string `json:"action"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

type serverMetrics struct {
	RAMUsedMb     int     `json:"ram_used_mb"`
	CPUPercent    float64 `json:"cpu_percent"`
	UptimeSeconds int64   `json:"uptime_seconds"`
	PlayersOnline *int    `json:"players_online"`
	PlayersMax    *int    `json:"players_max"`
}

type serverResponse struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Core         string         `json:"core"`
	Version      string         `json:"version"`
	Kind         string         `json:"kind"`
	Status       string         `json:"status"`
	RAMMb        int            `json:"ram_mb"`
	Port         int            `json:"port"`
	JavaArgs     string         `json:"java_args"`
	OwnerID      string         `json:"owner_id"`
	AutoStart    bool           `json:"auto_start"`
	AutoRestart  bool           `json:"auto_restart"`
	EULAAccepted bool           `json:"eula_accepted"`
	CreatedAt    time.Time      `json:"created_at"`
	Metrics      *serverMetrics `json:"metrics,omitempty"`
}

func toServerResponse(s *store.Server) serverResponse {
	return serverResponse{
		ID: s.ID, Name: s.Name, Core: s.Core, Version: s.Version, Kind: s.Kind,
		Status: s.Status, RAMMb: s.RAMMb, Port: s.Port, JavaArgs: s.JavaArgs,
		OwnerID: s.OwnerID, AutoStart: s.AutoStart, AutoRestart: s.AutoRestart,
		EULAAccepted: s.EULAAccepted, CreatedAt: s.CreatedAt,
	}
}

// --- handlers ---

// handleListServers serves GET /servers.
func (a *API) handleListServers(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFrom(r.Context())
	if !ok || !principal.HasScope(ScopeServersRead) {
		writeError(w, http.StatusForbidden, CodeForbidden, "token is missing the "+ScopeServersRead+" scope")
		return
	}

	filter := store.ServerFilter{
		Status: r.URL.Query().Get("status"),
		Core:   r.URL.Query().Get("core"),
	}
	// Admins see everything; everyone else sees only their own servers.
	if !principal.IsAdmin() {
		filter.OwnerID = principal.UserID
	}

	servers, err := a.store.Servers.List(r.Context(), filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not list servers")
		return
	}

	items := make([]serverResponse, 0, len(servers))
	for _, s := range servers {
		item := toServerResponse(s)
		item.Status = a.liveStatus(r.Context(), s)
		items = append(items, item)
	}
	writeJSON(w, http.StatusOK, listResponse[serverResponse]{Items: items})
}

// handleGetServer serves GET /servers/{id}, including live metrics.
func (a *API) handleGetServer(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	if _, ok := a.authorizeServer(w, r, serverID, ScopeServersRead); !ok {
		return
	}

	server, err := a.store.Servers.GetByID(r.Context(), serverID)
	if err != nil {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "server not found")
		return
	}

	resp := toServerResponse(server)
	resp.Status = a.liveStatus(r.Context(), server)
	resp.Metrics = a.metricsFor(r.Context(), server, resp.Status)
	writeJSON(w, http.StatusOK, resp)
}

// handleCreateServer serves POST /servers.
func (a *API) handleCreateServer(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFrom(r.Context())
	if !ok || !principal.HasScope(ScopeServersWrite) {
		writeError(w, http.StatusForbidden, CodeForbidden, "token is missing the "+ScopeServersWrite+" scope")
		return
	}

	var req createServerRequest
	if !decodeBody(w, r, &req) {
		return
	}

	name, err := normalizeServerName(req.Name)
	if err != nil {
		writeFieldError(w, "name", err.Error())
		return
	}
	if strings.TrimSpace(req.Core) == "" {
		writeFieldError(w, "core", "a core is required")
		return
	}
	if strings.TrimSpace(req.Version) == "" {
		writeFieldError(w, "version", "a version is required")
		return
	}
	// Accepting Mojang's EULA is the operator's decision, not a default the
	// panel can make on their behalf.
	if !req.EULAAccepted {
		writeErrorDetails(w, http.StatusUnprocessableEntity, CodeValidationFailed,
			"the Mojang EULA must be accepted to create a server",
			map[string]any{"field": "eula_accepted", "reason": "required"})
		return
	}
	if req.RAMMb < MinRAMMb || req.RAMMb > MaxRAMMb {
		writeFieldError(w, "ram_mb", "ram_mb must be between 512 and 524288")
		return
	}
	kind := req.Kind
	if kind == "" {
		kind = store.KindServer
	}
	if kind != store.KindServer && kind != store.KindProxy && kind != store.KindBedrock {
		writeFieldError(w, "kind", "kind must be server, proxy or bedrock")
		return
	}

	if ok := a.enforceUserLimits(w, r, principal, req.RAMMb, allChecks); !ok {
		return
	}

	port, ok := a.resolvePort(w, r, req.Port)
	if !ok {
		return
	}

	server := &store.Server{
		OwnerID: principal.UserID, Name: name, Core: req.Core, Version: req.Version,
		Kind: kind, Status: string(runner.StatusStopped), RAMMb: req.RAMMb, Port: port,
		JavaArgs: req.JavaArgs, EULAAccepted: true,
		AutoStart: req.AutoStart, AutoRestart: req.AutoRestart,
	}
	server.ID = store.NewID()
	// Stored relative to the data directory; see serverDir for why.
	server.Dir = path.Join(serversSubdir, server.ID)
	dir := a.serverDir(server)

	if err := os.MkdirAll(dir, 0o750); err != nil {
		a.log.Error("creating the server directory failed", slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not create the server directory")
		return
	}

	if err := a.store.Servers.Create(r.Context(), server); err != nil {
		// The directory would otherwise be orphaned by a failed insert.
		_ = os.RemoveAll(dir)

		if errors.Is(err, store.ErrPortInUse) {
			writeErrorDetails(w, http.StatusConflict, "port_in_use",
				"port is already assigned to another server",
				map[string]any{"field": "port", "reason": "already_in_use"})
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not create the server")
		return
	}

	// Writing eula.txt is what the server itself checks on first start;
	// recording the flag in the database alone would not be enough.
	eulaPath := filepath.Join(dir, "eula.txt")
	eulaBody := "# Generated by Mirocraft after the operator accepted the EULA through the API.\neula=true\n"
	if err := os.WriteFile(eulaPath, []byte(eulaBody), 0o640); err != nil {
		a.log.Warn("writing eula.txt failed", slog.String("error", err.Error()))
	}

	a.audit(r, principal.UserID, "server.create", server.ID, server.Name)
	writeJSON(w, http.StatusCreated, toServerResponse(server))
}

// limitChecks selects which allowances enforceUserLimits applies.
type limitChecks int

const (
	// allChecks applies both the server count and the memory allowance.
	allChecks limitChecks = iota
	// skipCountCheck applies only the memory allowance, for changes to a
	// server that already exists and is therefore already counted.
	skipCountCheck
)

// enforceUserLimits checks the caller's server count and RAM allowance, where
// ramMb is the additional memory the request would allocate.
func (a *API) enforceUserLimits(w http.ResponseWriter, r *http.Request, principal *Principal, ramMb int, checks limitChecks) bool {
	user, err := a.store.Users.GetByID(r.Context(), principal.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not read the account")
		return false
	}

	if checks == allChecks && user.MaxServers > store.Unlimited {
		count, err := a.store.Servers.CountByOwner(r.Context(), user.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, CodeInternalError, "could not count servers")
			return false
		}
		if count >= user.MaxServers {
			writeErrorDetails(w, http.StatusUnprocessableEntity, "insufficient_resources",
				"you have reached your server limit",
				map[string]any{"limit": user.MaxServers, "used": count})
			return false
		}
	}

	if user.MaxRAMMb > store.Unlimited {
		used, err := a.store.Servers.UsedRAMByOwner(r.Context(), user.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, CodeInternalError, "could not sum allocated ram")
			return false
		}
		if used+ramMb > user.MaxRAMMb {
			writeErrorDetails(w, http.StatusUnprocessableEntity, "insufficient_resources",
				"this would exceed your memory allowance",
				map[string]any{"limit_mb": user.MaxRAMMb, "used_mb": used, "requested_mb": ramMb})
			return false
		}
	}

	return true
}

// resolvePort validates an explicit port or allocates one.
func (a *API) resolvePort(w http.ResponseWriter, r *http.Request, requested int) (int, bool) {
	if requested == 0 {
		port, err := a.store.Servers.AllocatePort(r.Context(), a.portFrom, a.portTo)
		if err != nil {
			writeError(w, http.StatusConflict, "insufficient_resources",
				"no free port left in the configured range")
			return 0, false
		}
		return port, true
	}

	if requested < MinPort || requested > MaxPort {
		writeFieldError(w, "port", "port must be between 1024 and 65535")
		return 0, false
	}
	inUse, err := a.store.Servers.PortInUse(r.Context(), requested)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not check the port")
		return 0, false
	}
	if inUse {
		writeErrorDetails(w, http.StatusConflict, "port_in_use",
			"port is already assigned to another server",
			map[string]any{"field": "port", "reason": "already_in_use"})
		return 0, false
	}
	return requested, true
}

// handlePatchServer serves PATCH /servers/{id}.
func (a *API) handlePatchServer(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	principal, ok := a.authorizeServer(w, r, serverID, ScopeServersWrite)
	if !ok {
		return
	}

	var req patchServerRequest
	if !decodeBody(w, r, &req) {
		return
	}

	server, err := a.store.Servers.GetByID(r.Context(), serverID)
	if err != nil {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "server not found")
		return
	}

	if req.Name != nil {
		name, err := normalizeServerName(*req.Name)
		if err != nil {
			writeFieldError(w, "name", err.Error())
			return
		}
		server.Name = name
	}
	if req.RAMMb != nil {
		if *req.RAMMb < MinRAMMb || *req.RAMMb > MaxRAMMb {
			writeFieldError(w, "ram_mb", "ram_mb must be between 512 and 524288")
			return
		}
		// The quota has to be checked here too, or it is no quota at all: a
		// user could create a small server within their allowance and then
		// patch it up to the global maximum. What is being added is the
		// difference, since this server's current allocation is already
		// counted in the total.
		if delta := *req.RAMMb - server.RAMMb; delta > 0 {
			if !a.enforceUserLimits(w, r, principal, delta, skipCountCheck) {
				return
			}
		}
		server.RAMMb = *req.RAMMb
	}
	if req.JavaArgs != nil {
		server.JavaArgs = *req.JavaArgs
	}
	if req.AutoStart != nil {
		server.AutoStart = *req.AutoStart
	}
	if req.AutoRestart != nil {
		server.AutoRestart = *req.AutoRestart
	}

	if err := a.store.Servers.Update(r.Context(), server); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not update the server")
		return
	}

	a.audit(r, principal.UserID, "server.update", server.ID, "")
	writeJSON(w, http.StatusOK, toServerResponse(server))
}

// handleDeleteServer serves DELETE /servers/{id}?confirm={name}.
//
// The confirmation is required because this destroys worlds: a mistyped id
// with no confirmation would be unrecoverable.
func (a *API) handleDeleteServer(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	principal, ok := a.authorizeServer(w, r, serverID, ScopeServersWrite)
	if !ok {
		return
	}

	server, err := a.store.Servers.GetByID(r.Context(), serverID)
	if err != nil {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "server not found")
		return
	}

	if r.URL.Query().Get("confirm") != server.Name {
		writeErrorDetails(w, http.StatusBadRequest, CodeValidationFailed,
			"pass ?confirm=<server name> to delete a server and its data",
			map[string]any{"field": "confirm", "reason": "must equal the server name"})
		return
	}

	// A running server is stopped first, so its files are not pulled out from
	// under a live process.
	if status, err := a.serverStatus(r.Context(), serverID); err == nil && status.IsActive() {
		if err := a.lifecycle.Kill(r.Context(), serverID); err != nil {
			a.log.Warn("killing the server before deletion failed",
				slog.String("server_id", serverID), slog.String("error", err.Error()))
		}
	}

	if err := a.store.Servers.Delete(r.Context(), serverID); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not delete the server")
		return
	}
	if dir := a.serverDir(server); dir != "" {
		if err := os.RemoveAll(dir); err != nil {
			a.log.Warn("removing the server directory failed",
				slog.String("dir", dir), slog.String("error", err.Error()))
		}
	}

	a.audit(r, principal.UserID, "server.delete", serverID, server.Name)
	w.WriteHeader(http.StatusNoContent)
}

// handlePower serves POST /servers/{id}/power.
func (a *API) handlePower(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	principal, ok := a.authorizeServer(w, r, serverID, ScopeServersPower)
	if !ok {
		return
	}

	var req powerRequest
	if !decodeBody(w, r, &req) {
		return
	}

	server, err := a.store.Servers.GetByID(r.Context(), serverID)
	if err != nil {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "server not found")
		return
	}

	timeout := time.Duration(req.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = a.stopTimeout
	}

	if a.lifecycle == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternalError,
			"no runner is configured on this node")
		return
	}

	status, _ := a.serverStatus(r.Context(), serverID)
	switch req.Action {
	case ActionStart:
		if status.IsActive() {
			writeError(w, http.StatusConflict, "server_already_running", "server is already running")
			return
		}
	case ActionStop, ActionKill:
		if !status.IsActive() {
			writeError(w, http.StatusConflict, CodeServerNotRunning, "server is not running")
			return
		}
	case ActionRestart:
		// Restart works from either state: it starts a stopped server.
	default:
		writeFieldError(w, "action", "action must be start, stop, restart or kill")
		return
	}

	task := a.tasks.start("power."+req.Action, serverID, principal.UserID,
		func(ctx context.Context) error {
			return a.runPower(ctx, server, req.Action, timeout)
		})

	a.audit(r, principal.UserID, "server.power."+req.Action, serverID, "")
	writeJSON(w, http.StatusAccepted, taskAcceptedResponse{TaskID: task.ID})
}

// runPower performs the requested lifecycle change and keeps the stored status
// in step with it.
func (a *API) runPower(ctx context.Context, server *store.Server, action string, timeout time.Duration) error {
	switch action {
	case ActionStart:
		return a.startServer(ctx, server)
	case ActionStop:
		if err := a.lifecycle.Stop(ctx, server.ID, timeout); err != nil {
			return err
		}
	case ActionKill:
		if err := a.lifecycle.Kill(ctx, server.ID); err != nil {
			return err
		}
	case ActionRestart:
		if status, err := a.serverStatus(ctx, server.ID); err == nil && status.IsActive() {
			if err := a.lifecycle.Stop(ctx, server.ID, timeout); err != nil {
				return err
			}
		}
		return a.startServer(ctx, server)
	}

	a.recordStatus(ctx, server.ID, string(runner.StatusStopped))
	return nil
}

func (a *API) startServer(ctx context.Context, server *store.Server) error {
	launch := &runner.Server{
		ID: server.ID, Name: server.Name, Dir: a.serverDir(server),
		Core: server.Core, Version: server.Version, RAMMb: server.RAMMb,
		JarName: server.JarName, JavaArgs: splitJavaArgs(server.JavaArgs),
	}

	// Provisioning downloads the core and the Java runtime on a cold start,
	// which is why it happens inside the task rather than in the request:
	// roughly 110 MB the first time, and cached afterwards.
	if a.provisioner != nil {
		prepared, err := a.provisioner.Prepare(ctx, server, a.serverDir(server))
		if err != nil {
			a.recordStatus(ctx, server.ID, string(runner.StatusCrashed))
			return err
		}
		launch.JarName = prepared.JarName
		launch.JavaBin = prepared.JavaBin

		// Remembered so the panel can show what is actually installed, and so
		// a later start knows the jar without asking upstream again.
		if server.JarName != prepared.JarName {
			server.JarName = prepared.JarName
			if err := a.store.Servers.Update(ctx, server); err != nil {
				a.log.Warn("recording the installed jar failed",
					slog.String("server_id", server.ID), slog.String("error", err.Error()))
			}
		}
	}

	if err := a.lifecycle.Start(ctx, launch); err != nil {
		a.recordStatus(ctx, server.ID, string(runner.StatusCrashed))
		return err
	}
	a.recordStatus(ctx, server.ID, string(runner.StatusRunning))

	// One watcher per running server turns its console and status into events
	// for the bus. Started here rather than per client, so ten browser tabs
	// cost one subscription rather than ten.
	//
	// The watcher's context outlives the request that started the server; it
	// ends when the server's own subscriptions close, which the runner does
	// when the process exits.
	a.WatchServer(context.WithoutCancel(ctx), server.ID, server.OwnerID)
	return nil
}

func (a *API) recordStatus(ctx context.Context, serverID, status string) {
	if err := a.store.Servers.UpdateStatus(ctx, serverID, status); err != nil {
		a.log.Warn("recording server status failed",
			slog.String("server_id", serverID), slog.String("error", err.Error()))
	}
}

// splitJavaArgs turns the stored argument string into a slice. Quoting is not
// supported on purpose: JVM flags do not contain spaces, and a half-working
// quote parser is worse than none.
func splitJavaArgs(args string) []string {
	fields := strings.Fields(args)
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// serversSubdir is where server directories live under the data directory.
const serversSubdir = "servers"

// serverDir returns where a server's files actually are.
//
// The location is derived from the data directory rather than read from the
// database, because an absolute path stored at creation time stops being true
// the moment the data directory moves — which happens when an operator
// relocates it, restores a backup onto another host, or changes --data-dir.
// Every server would then point at a directory that no longer exists.
//
// Records written before this change still hold an absolute path; those are
// honoured as they are, so an existing installation keeps working.
func (a *API) serverDir(s *store.Server) string {
	if s == nil {
		return ""
	}
	if s.Dir != "" && filepath.IsAbs(s.Dir) {
		return s.Dir
	}
	return filepath.Join(a.dataDir, serversSubdir, s.ID)
}

// serverStatus asks the runner for the current state. The nil check is not
// defensive clutter: Options.Lifecycle is optional, and every other reader of
// it guards, so the power and delete paths must too rather than panicking in
// a goroutine that has no recover above it.
func (a *API) serverStatus(ctx context.Context, serverID string) (runner.Status, error) {
	if a.lifecycle == nil {
		return runner.StatusStopped, runner.ErrRunnerUnavailable
	}
	return a.lifecycle.Status(ctx, serverID)
}

// liveStatus reports what the runner says, and only falls back to the stored
// value when there is no runner at all.
//
// It deliberately does not fall back when the runner simply does not know the
// server: that means the process is not one this daemon manages, and claiming
// "running" from a stale database row would show an operator a server whose
// stop button cannot work. ReconcileServers turns those rows honest at
// startup; this keeps them honest afterwards.
func (a *API) liveStatus(ctx context.Context, server *store.Server) string {
	if a.lifecycle == nil {
		return server.Status
	}
	status, err := a.lifecycle.Status(ctx, server.ID)
	if err != nil {
		if errors.Is(err, runner.ErrServerNotFound) {
			return string(runner.StatusStopped)
		}
		return server.Status
	}
	return string(status)
}

// ReconcileServers makes the stored statuses true again after a restart.
//
// A server is a direct child process, so when the daemon exits its children
// are orphaned: they keep running, but no new daemon can reach their stdin or
// read their console. The database still says "running", which would show an
// operator a server they cannot stop, whose port is taken and whose world is
// locked by a process nothing manages.
//
// Marking them stopped is the honest half. Killing the orphan is the other
// half and needs the process tree handling from task 1.6, so for now the
// operator is told plainly.
func (a *API) ReconcileServers(ctx context.Context) {
	servers, err := a.store.Servers.List(ctx, store.ServerFilter{})
	if err != nil {
		a.log.Warn("reconciling server statuses failed", slog.String("error", err.Error()))
		return
	}

	for _, server := range servers {
		if !runner.Status(server.Status).IsActive() {
			continue
		}
		if a.lifecycle != nil {
			if _, err := a.lifecycle.Status(ctx, server.ID); err == nil {
				continue // this daemon does manage it
			}
		}

		a.log.Warn("a server was left running by a previous daemon and is no longer manageable; "+
			"it is now recorded as stopped, but the process may still be alive and holding its port",
			slog.String("server_id", server.ID),
			slog.String("name", server.Name),
			slog.Int("port", server.Port))

		a.recordStatus(ctx, server.ID, string(runner.StatusStopped))
	}
}

// metricsFor gathers process statistics and, for a running server, the player
// counts reported by a list ping.
func (a *API) metricsFor(ctx context.Context, server *store.Server, status string) *serverMetrics {
	if a.lifecycle == nil || !runner.Status(status).IsActive() {
		return nil
	}

	stats, err := a.lifecycle.Stats(ctx, server.ID)
	if err != nil {
		return nil
	}

	metrics := &serverMetrics{
		RAMUsedMb:     int(stats.RAMBytes / (1024 * 1024)),
		CPUPercent:    stats.CPUPercent,
		UptimeSeconds: int64(stats.Uptime.Seconds()),
	}

	// Player counts come from the game, not the process, so a server that is
	// still loading simply reports none rather than failing the whole request.
	pingCtx, cancel := context.WithTimeout(ctx, mcping.DefaultTimeout)
	defer cancel()

	ping := a.ping
	if ping == nil {
		ping = mcping.Ping
	}
	if pong, err := ping(pingCtx, "127.0.0.1", server.Port); err == nil {
		online, max := pong.PlayersOnline, pong.PlayersMax
		metrics.PlayersOnline = &online
		metrics.PlayersMax = &max
	}

	return metrics
}

// normalizeServerName validates a name and returns the form to store.
//
// It returns the trimmed value rather than only validating it, for the same
// reason emails are normalized: validating a trimmed copy and storing the raw
// input would save a name that nothing else matches. Here the consequence is
// that DELETE ?confirm=<name> could never succeed, leaving the owner unable to
// remove their own server.
func normalizeServerName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("a name is required")
	}
	if len([]rune(name)) > MaxServerLen {
		return "", errors.New("name must be at most 64 characters")
	}
	// The name becomes part of a path, so anything that could traverse or
	// confuse a filesystem is refused rather than escaped.
	if !serverNamePattern.MatchString(name) {
		return "", errors.New("name may contain only letters, digits, spaces, hyphens and underscores, " +
			"and must start and end with a letter or a digit")
	}
	return name, nil
}
