package api

import (
	"context"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/collybia/mirocraft/internal/backup"
	"github.com/collybia/mirocraft/internal/store"
)

// Backup limits.
const (
	// MaxKeepLast is the largest retention a schedule may ask for.
	MaxKeepLast = 365
	// DefaultKeepLast is what a schedule gets when it does not say.
	DefaultKeepLast = 7
)

// cronParser accepts the five-field form an operator expects, with the
// descriptors (@daily and friends) that are far easier to get right.
var cronParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// --- wire types ---

type backupResponse struct {
	ID        string    `json:"id"`
	ServerID  string    `json:"server_id"`
	Note      string    `json:"note"`
	State     string    `json:"state"`
	SizeBytes int64     `json:"size_bytes"`
	CreatedAt time.Time `json:"created_at"`
}

type createBackupRequest struct {
	Note string `json:"note"`
}

type scheduleResponse struct {
	Cron      string     `json:"cron"`
	KeepLast  int        `json:"keep_last"`
	Enabled   bool       `json:"enabled"`
	LastRunAt *time.Time `json:"last_run_at"`
	NextRunAt *time.Time `json:"next_run_at"`
}

type putScheduleRequest struct {
	Cron     string `json:"cron"`
	KeepLast *int   `json:"keep_last"`
	Enabled  *bool  `json:"enabled"`
}

func toBackupResponse(b *store.Backup) backupResponse {
	return backupResponse{
		ID: b.ID, ServerID: b.ServerID, Note: b.Note, State: b.State,
		SizeBytes: b.SizeBytes, CreatedAt: b.CreatedAt,
	}
}

// --- handlers ---

// handleListBackups serves GET /servers/{id}/backups.
func (a *API) handleListBackups(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	if _, ok := a.authorizeServer(w, r, serverID, ScopeBackupsRead); !ok {
		return
	}

	backups, err := a.store.Backups.ListByServer(r.Context(), serverID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not list backups")
		return
	}

	items := make([]backupResponse, 0, len(backups))
	for _, b := range backups {
		items = append(items, toBackupResponse(b))
	}
	writeJSON(w, http.StatusOK, listResponse[backupResponse]{Items: items})
}

// handleCreateBackup serves POST /servers/{id}/backups.
func (a *API) handleCreateBackup(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	principal, ok := a.authorizeServer(w, r, serverID, ScopeBackupsWrite)
	if !ok {
		return
	}
	if a.backups == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternalError, "backups are not configured")
		return
	}

	var req createBackupRequest
	if !decodeBody(w, r, &req) {
		return
	}

	server, err := a.store.Servers.GetByID(r.Context(), serverID)
	if err != nil {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "server not found")
		return
	}

	// Two archivers walking the same directory produce two archives that each
	// caught it mid-write, so a second request is refused rather than queued.
	running, err := a.store.Backups.RunningFor(r.Context(), serverID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not check running backups")
		return
	}
	if running {
		writeError(w, http.StatusConflict, "backup_in_progress",
			"a backup of this server is already running")
		return
	}

	record := &store.Backup{ServerID: serverID, Note: req.Note, State: store.BackupPending}
	if err := a.store.Backups.Create(r.Context(), record); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not record the backup")
		return
	}

	task := a.tasks.start("backup.create", serverID, principal.UserID,
		func(ctx context.Context) error {
			return a.runBackup(ctx, server, record)
		})

	a.audit(r, principal.UserID, "backup.create", record.ID, req.Note)
	writeJSON(w, http.StatusAccepted, taskAcceptedResponse{TaskID: task.ID})
}

// saveFlushWait is how long the server is given to write the world out after
// being asked to. Flushing a large world takes a moment, and archiving before
// it finishes captures a world half-written.
const saveFlushWait = 3 * time.Second

// runBackup archives the server and records the outcome.
func (a *API) runBackup(ctx context.Context, server *store.Server, record *store.Backup) error {
	if err := a.store.Backups.SetState(ctx, record.ID, store.BackupRunning); err != nil {
		return err
	}

	// A running server keeps the world in memory and writes it out when it
	// feels like it, so archiving straight away captures a mix of old files
	// and new ones. Asking it to flush and then hold off makes the snapshot
	// consistent; this is what `save-off` exists for.
	a.quiesce(ctx, server.ID)
	defer a.resumeSaving(ctx, server.ID)

	path, size, err := a.backups.Create(ctx, server.ID, record.ID, a.serverDir(server))
	if err != nil {
		// The record is kept as failed rather than deleted: an operator who
		// scheduled backups needs to see that one did not work.
		if setErr := a.store.Backups.SetState(ctx, record.ID, store.BackupFailed); setErr != nil {
			a.log.Warn("recording the failed backup state failed",
				slog.String("backup_id", record.ID), slog.String("error", setErr.Error()))
		}
		return err
	}

	if err := a.store.Backups.Finish(ctx, record.ID, store.BackupDone, path, size); err != nil {
		return err
	}

	a.pruneBackups(ctx, server.ID)
	return nil
}

// quiesce asks a running server to flush its world and stop saving.
//
// Best effort throughout: a server that is stopped, still starting, or simply
// does not understand the commands still gets backed up. The alternative —
// refusing to back up unless the game cooperates — would leave an operator
// with no backup at all.
func (a *API) quiesce(ctx context.Context, serverID string) {
	if a.console == nil {
		return
	}
	if status, err := a.serverStatus(ctx, serverID); err != nil || !status.IsActive() {
		return
	}

	if err := a.console.SendCommand(ctx, serverID, "save-off"); err != nil {
		a.log.Debug("save-off was not accepted", slog.String("error", err.Error()))
		return
	}
	if err := a.console.SendCommand(ctx, serverID, "save-all flush"); err != nil {
		a.log.Debug("save-all was not accepted", slog.String("error", err.Error()))
	}

	select {
	case <-ctx.Done():
	case <-time.After(saveFlushWait):
	}
}

// resumeSaving switches world saving back on.
//
// This must run whatever happened above: a server left with saving disabled
// silently loses everything since the backup when it next stops.
func (a *API) resumeSaving(ctx context.Context, serverID string) {
	if a.console == nil {
		return
	}
	if status, err := a.serverStatus(ctx, serverID); err != nil || !status.IsActive() {
		return
	}
	if err := a.console.SendCommand(ctx, serverID, "save-on"); err != nil {
		a.log.Warn("could not re-enable world saving after a backup",
			slog.String("server_id", serverID), slog.String("error", err.Error()))
	}
}

// pruneBackups applies the schedule's retention, if there is one.
func (a *API) pruneBackups(ctx context.Context, serverID string) {
	schedule, err := a.store.Backups.GetSchedule(ctx, serverID)
	if err != nil {
		return // no schedule, so nothing to prune to
	}

	stale, err := a.store.Backups.Prune(ctx, serverID, schedule.KeepLast)
	if err != nil {
		a.log.Warn("pruning backups failed", slog.String("error", err.Error()))
		return
	}

	for _, b := range stale {
		if err := a.backups.Delete(serverID, b.ID); err != nil {
			a.log.Warn("deleting a pruned archive failed",
				slog.String("backup_id", b.ID), slog.String("error", err.Error()))
			continue
		}
		if err := a.store.Backups.Delete(ctx, b.ID); err != nil {
			a.log.Warn("deleting a pruned record failed",
				slog.String("backup_id", b.ID), slog.String("error", err.Error()))
		}
	}
}

// handleDownloadBackup serves GET /servers/{id}/backups/{bid}/download.
func (a *API) handleDownloadBackup(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	if _, ok := a.authorizeServer(w, r, serverID, ScopeBackupsRead); !ok {
		return
	}

	record, server, ok := a.backupRecord(w, r, serverID)
	if !ok {
		return
	}
	if record.State != store.BackupDone {
		writeError(w, http.StatusConflict, CodeValidationFailed,
			"the backup is not finished")
		return
	}

	file, info, err := a.backups.Open(serverID, record.ID)
	if err != nil {
		if errors.Is(err, backup.ErrNotFound) {
			writeError(w, http.StatusNotFound, CodeServerNotFound, "the archive is missing")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not open the archive")
		return
	}
	defer func() { _ = file.Close() }()

	name := backup.SuggestName(server.Name, record.CreatedAt)
	w.Header().Set("Content-Disposition",
		mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("Content-Type", "application/zip")

	http.ServeContent(w, r, name, info.ModTime(), file)
}

// handleRestoreBackup serves POST /servers/{id}/backups/{bid}/restore.
func (a *API) handleRestoreBackup(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	principal, ok := a.authorizeServer(w, r, serverID, ScopeBackupsWrite)
	if !ok {
		return
	}
	if a.backups == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternalError, "backups are not configured")
		return
	}

	record, server, ok := a.backupRecord(w, r, serverID)
	if !ok {
		return
	}
	if record.State != store.BackupDone {
		writeError(w, http.StatusConflict, CodeValidationFailed, "the backup is not finished")
		return
	}

	// Restoring wipes the directory a running server has open. On Windows it
	// would fail halfway through and leave the server with half a world; on
	// Linux it would succeed and the running process would then write the old
	// world back out. Both are worse than refusing.
	if status, err := a.serverStatus(r.Context(), serverID); err == nil && status.IsActive() {
		writeError(w, http.StatusConflict, "server_already_running",
			"stop the server before restoring a backup")
		return
	}

	task := a.tasks.start("backup.restore", serverID, principal.UserID,
		func(ctx context.Context) error {
			path := a.backups.Path(serverID, record.ID)
			return a.backups.Restore(ctx, path, a.serverDir(server))
		})

	a.audit(r, principal.UserID, "backup.restore", record.ID, "")
	writeJSON(w, http.StatusAccepted, taskAcceptedResponse{TaskID: task.ID})
}

// handleDeleteBackup serves DELETE /servers/{id}/backups/{bid}.
func (a *API) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	principal, ok := a.authorizeServer(w, r, serverID, ScopeBackupsWrite)
	if !ok {
		return
	}

	record, _, ok := a.backupRecord(w, r, serverID)
	if !ok {
		return
	}
	if record.State == store.BackupRunning || record.State == store.BackupPending {
		writeError(w, http.StatusConflict, "backup_in_progress",
			"the backup is still being written")
		return
	}

	if a.backups != nil {
		if err := a.backups.Delete(serverID, record.ID); err != nil {
			a.log.Warn("deleting the archive failed", slog.String("error", err.Error()))
		}
	}
	if err := a.store.Backups.Delete(r.Context(), record.ID); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not delete the backup")
		return
	}

	a.audit(r, principal.UserID, "backup.delete", record.ID, "")
	w.WriteHeader(http.StatusNoContent)
}

// handleGetSchedule serves GET /servers/{id}/backups/schedule.
func (a *API) handleGetSchedule(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	if _, ok := a.authorizeServer(w, r, serverID, ScopeBackupsRead); !ok {
		return
	}

	schedule, err := a.store.Backups.GetSchedule(r.Context(), serverID)
	if err != nil {
		// No schedule is a normal state, not an error: the panel shows the
		// form empty and disabled.
		writeJSON(w, http.StatusOK, scheduleResponse{KeepLast: DefaultKeepLast})
		return
	}

	writeJSON(w, http.StatusOK, toScheduleResponse(schedule))
}

// handlePutSchedule serves PUT /servers/{id}/backups/schedule.
func (a *API) handlePutSchedule(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	principal, ok := a.authorizeServer(w, r, serverID, ScopeBackupsWrite)
	if !ok {
		return
	}

	var req putScheduleRequest
	if !decodeBody(w, r, &req) {
		return
	}

	if strings.TrimSpace(req.Cron) == "" {
		writeFieldError(w, "cron", "a cron expression is required")
		return
	}
	// Validated here rather than at run time: a schedule that cannot be
	// parsed would otherwise be accepted and then silently never fire.
	if _, err := cronParser.Parse(req.Cron); err != nil {
		writeFieldError(w, "cron",
			"the cron expression is not valid: "+err.Error())
		return
	}

	keep := DefaultKeepLast
	if req.KeepLast != nil {
		keep = *req.KeepLast
	}
	if keep < 1 || keep > MaxKeepLast {
		writeFieldError(w, "keep_last", "keep_last must be between 1 and 365")
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	schedule := &store.BackupSchedule{
		ServerID: serverID, Cron: req.Cron, KeepLast: keep, Enabled: enabled,
	}
	if existing, err := a.store.Backups.GetSchedule(r.Context(), serverID); err == nil {
		schedule.CreatedAt = existing.CreatedAt
		schedule.LastRunAt = existing.LastRunAt
	}

	if err := a.store.Backups.SetSchedule(r.Context(), schedule); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not save the schedule")
		return
	}

	a.audit(r, principal.UserID, "backup.schedule", serverID, req.Cron)
	writeJSON(w, http.StatusOK, toScheduleResponse(schedule))
}

func toScheduleResponse(s *store.BackupSchedule) scheduleResponse {
	out := scheduleResponse{
		Cron: s.Cron, KeepLast: s.KeepLast, Enabled: s.Enabled, LastRunAt: s.LastRunAt,
	}
	if schedule, err := cronParser.Parse(s.Cron); err == nil && s.Enabled {
		next := schedule.Next(time.Now())
		out.NextRunAt = &next
	}
	return out
}

// backupRecord loads the backup named in the path and checks it belongs to
// the server in the path.
//
// Without the ownership check, a backup id from another server would be
// downloadable by anyone who can reach their own server's endpoint.
func (a *API) backupRecord(w http.ResponseWriter, r *http.Request, serverID string) (*store.Backup, *store.Server, bool) {
	record, err := a.store.Backups.GetByID(r.Context(), r.PathValue("bid"))
	if err != nil || record.ServerID != serverID {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "backup not found")
		return nil, nil, false
	}

	server, err := a.store.Servers.GetByID(r.Context(), serverID)
	if err != nil {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "server not found")
		return nil, nil, false
	}
	return record, server, true
}

// --- the scheduler ---

// RunBackupSchedules fires due backups until ctx is cancelled.
//
// It ticks rather than sleeping until the next due time, so a schedule added
// or changed while the daemon runs is picked up without restarting anything.
func (a *API) RunBackupSchedules(ctx context.Context, interval time.Duration) {
	if a.backups == nil {
		return
	}
	if interval <= 0 {
		interval = time.Minute
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			a.fireDueBackups(ctx, now)
		}
	}
}

// fireDueBackups starts a backup for every schedule whose time has come.
func (a *API) fireDueBackups(ctx context.Context, now time.Time) {
	schedules, err := a.store.Backups.ListEnabledSchedules(ctx)
	if err != nil {
		a.log.Warn("reading backup schedules failed", slog.String("error", err.Error()))
		return
	}

	for _, schedule := range schedules {
		parsed, err := cronParser.Parse(schedule.Cron)
		if err != nil {
			a.log.Warn("a stored cron expression is invalid",
				slog.String("server_id", schedule.ServerID), slog.String("cron", schedule.Cron))
			continue
		}

		// Due when the next run after the last one has already passed. Using
		// the recorded last run rather than a timer means a daemon that was
		// down over a scheduled time takes the backup once on the next tick,
		// instead of skipping the day entirely.
		from := schedule.CreatedAt
		if schedule.LastRunAt != nil {
			from = *schedule.LastRunAt
		}
		if parsed.Next(from).After(now) {
			continue
		}

		a.startScheduledBackup(ctx, schedule, now)
	}
}

func (a *API) startScheduledBackup(ctx context.Context, schedule *store.BackupSchedule, now time.Time) {
	server, err := a.store.Servers.GetByID(ctx, schedule.ServerID)
	if err != nil {
		return
	}

	running, err := a.store.Backups.RunningFor(ctx, schedule.ServerID)
	if err != nil || running {
		return
	}

	// Recorded before the work starts, so a backup that takes longer than the
	// tick interval is not started again on the next tick.
	if err := a.store.Backups.MarkScheduleRun(ctx, schedule.ServerID, now); err != nil {
		a.log.Warn("recording the schedule run failed", slog.String("error", err.Error()))
		return
	}

	record := &store.Backup{
		ServerID: schedule.ServerID, Note: "scheduled", State: store.BackupPending,
	}
	if err := a.store.Backups.Create(ctx, record); err != nil {
		a.log.Warn("recording a scheduled backup failed", slog.String("error", err.Error()))
		return
	}

	a.log.Info("scheduled backup starting",
		slog.String("server_id", schedule.ServerID), slog.String("cron", schedule.Cron))

	a.tasks.start("backup.scheduled", schedule.ServerID, server.OwnerID,
		func(taskCtx context.Context) error {
			return a.runBackup(taskCtx, server, record)
		})
}
