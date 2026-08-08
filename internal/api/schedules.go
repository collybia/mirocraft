package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/collybia/mirocraft/internal/runner"
	"github.com/collybia/mirocraft/internal/store"
)

// Limits on a schedule.
const (
	// MaxSchedulesPerServer bounds how many chains one server may carry.
	MaxSchedulesPerServer = 20
	// MaxScheduleActions bounds the length of one chain.
	MaxScheduleActions = 20
	// MaxWaitSeconds bounds a single wait.
	MaxWaitSeconds = 300
	// MaxChainWaitSeconds bounds every wait in a chain added together.
	//
	// A chain is executed inside a task, and a task is given ten minutes. A
	// chain that could out-wait its own deadline would be killed halfway
	// through — stopping a server it had already warned but never restarting
	// it — so the limit is set well inside that.
	MaxChainWaitSeconds = 300
	// MaxScheduleNameLen bounds the display name.
	MaxScheduleNameLen = 100
)

// --- wire types ---

type scheduleResponse struct {
	ID       string         `json:"id"`
	ServerID string         `json:"server_id"`
	Name     string         `json:"name"`
	Cron     string         `json:"cron"`
	Actions  []store.Action `json:"actions"`
	Enabled  bool           `json:"enabled"`

	LastRunAt *time.Time `json:"last_run_at"`
	// Always present, empty before the first run: it is the field a client
	// switches on, and one that is sometimes absent and sometimes empty makes
	// every caller handle two shapes of "has not run yet".
	LastStatus string     `json:"last_status"`
	LastError  string     `json:"last_error,omitempty"`
	NextRunAt  *time.Time `json:"next_run_at"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type createScheduleRequest struct {
	Name    string         `json:"name"`
	Cron    string         `json:"cron"`
	Actions []store.Action `json:"actions"`
	Enabled *bool          `json:"enabled"`
}

type patchScheduleRequest struct {
	Name    *string         `json:"name"`
	Cron    *string         `json:"cron"`
	Actions *[]store.Action `json:"actions"`
	Enabled *bool           `json:"enabled"`
}

func toScheduleResponse(s *store.Schedule) scheduleResponse {
	resp := scheduleResponse{
		ID: s.ID, ServerID: s.ServerID, Name: s.Name, Cron: s.Cron,
		Actions: s.Actions, Enabled: s.Enabled,
		LastRunAt: s.LastRunAt, LastStatus: s.LastStatus, LastError: s.LastError,
		CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt,
	}
	if resp.Actions == nil {
		resp.Actions = []store.Action{}
	}

	// Shown so an operator can check a cron expression means what they think
	// without waiting overnight to find out.
	if parsed, err := cronParser.Parse(s.Cron); err == nil && s.Enabled {
		next := parsed.Next(time.Now())
		resp.NextRunAt = &next
	}
	return resp
}

// --- validation ---

// scopeForAction is the scope a caller needs to put an action in a chain.
//
// A schedule is a stored instruction to act later, so composing one must
// require what performing it by hand would. Without this, servers:write alone
// would be a way to run arbitrary console commands and power the server off —
// the scopes would stop meaning anything.
var scopeForAction = map[string]string{
	store.ActionCommand: ScopeServersConsole,
	store.ActionPower:   ScopeServersPower,
	store.ActionBackup:  ScopeBackupsWrite,
	store.ActionWait:    "", // waiting needs nothing
}

// validateActions checks a chain and reports the first problem.
func validateActions(actions []store.Action) error {
	if len(actions) == 0 {
		return errors.New("a schedule needs at least one action")
	}
	if len(actions) > MaxScheduleActions {
		return fmt.Errorf("a schedule may have at most %d actions", MaxScheduleActions)
	}

	totalWait := 0
	for i, action := range actions {
		if _, known := scopeForAction[action.Type]; !known {
			return fmt.Errorf("action %d: type must be %s, %s, %s or %s",
				i+1, store.ActionCommand, store.ActionPower,
				store.ActionBackup, store.ActionWait)
		}

		switch action.Type {
		case store.ActionCommand:
			// Validated by the same rules the console applies, so a chain
			// cannot smuggle in a command the console would refuse.
			if err := runner.ValidateCommand(action.String("command")); err != nil {
				return fmt.Errorf("action %d: %w", i+1, err)
			}

		case store.ActionPower:
			switch action.String("action") {
			case ActionStart, ActionStop, ActionRestart, ActionKill:
			default:
				return fmt.Errorf("action %d: power action must be start, stop, restart or kill", i+1)
			}

		case store.ActionWait:
			seconds, ok := action.Int("seconds")
			if !ok || seconds <= 0 {
				return fmt.Errorf("action %d: wait needs a positive seconds", i+1)
			}
			if seconds > MaxWaitSeconds {
				return fmt.Errorf("action %d: a wait may be at most %d seconds", i+1, MaxWaitSeconds)
			}
			totalWait += seconds
			if totalWait > MaxChainWaitSeconds {
				return fmt.Errorf("action %d: the waits in a chain may total at most %d seconds",
					i+1, MaxChainWaitSeconds)
			}
		}
	}
	return nil
}

// scopesForActions reports a scope the caller is missing for a chain.
func missingScopeFor(principal *Principal, actions []store.Action) string {
	for _, action := range actions {
		scope := scopeForAction[action.Type]
		if scope != "" && !principal.HasScope(scope) {
			return scope
		}
	}
	return ""
}

func validateScheduleName(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", errors.New("a name is required")
	}
	if len([]rune(trimmed)) > MaxScheduleNameLen {
		return "", fmt.Errorf("a name may be at most %d characters", MaxScheduleNameLen)
	}
	return trimmed, nil
}

func validateCron(expr string) error {
	if strings.TrimSpace(expr) == "" {
		return errors.New("a cron expression is required")
	}
	// Accepted and then silently never firing would be the worst outcome, so
	// the expression is parsed here rather than at run time.
	if _, err := cronParser.Parse(expr); err != nil {
		return errors.New("the cron expression is not valid: " + err.Error())
	}
	return nil
}

// --- handlers ---

// handleListSchedules serves GET /servers/{id}/schedules.
func (a *API) handleListSchedules(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	if _, ok := a.authorizeServer(w, r, serverID, ScopeServersRead); !ok {
		return
	}

	schedules, err := a.store.Schedules.ListByServer(r.Context(), serverID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not list schedules")
		return
	}

	items := make([]scheduleResponse, 0, len(schedules))
	for _, s := range schedules {
		items = append(items, toScheduleResponse(s))
	}
	writeJSON(w, http.StatusOK, listResponse[scheduleResponse]{Items: items})
}

// handleCreateSchedule serves POST /servers/{id}/schedules.
func (a *API) handleCreateSchedule(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	principal, ok := a.authorizeServer(w, r, serverID, ScopeServersWrite)
	if !ok {
		return
	}

	var req createScheduleRequest
	if !decodeBody(w, r, &req) {
		return
	}

	name, err := validateScheduleName(req.Name)
	if err != nil {
		writeFieldError(w, "name", err.Error())
		return
	}
	if err := validateCron(req.Cron); err != nil {
		writeFieldError(w, "cron", err.Error())
		return
	}
	if err := validateActions(req.Actions); err != nil {
		writeFieldError(w, "actions", err.Error())
		return
	}
	if scope := missingScopeFor(principal, req.Actions); scope != "" {
		writeError(w, http.StatusForbidden, CodeForbidden,
			"token is missing the "+scope+" scope, which this chain's actions require")
		return
	}

	count, err := a.store.Schedules.CountByServer(r.Context(), serverID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not read schedules")
		return
	}
	if count >= MaxSchedulesPerServer {
		writeFieldError(w, "name", "this server already has the maximum number of schedules")
		return
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}

	schedule := &store.Schedule{
		ServerID: serverID, Name: name, Cron: req.Cron,
		Actions: req.Actions, Enabled: enabled,
	}
	if err := a.store.Schedules.Create(r.Context(), schedule); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not create the schedule")
		return
	}

	a.audit(r, principal.UserID, "schedule.create", serverID, schedule.Name)
	writeJSON(w, http.StatusCreated, toScheduleResponse(schedule))
}

// handlePatchSchedule serves PATCH /servers/{id}/schedules/{sid}.
func (a *API) handlePatchSchedule(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	principal, ok := a.authorizeServer(w, r, serverID, ScopeServersWrite)
	if !ok {
		return
	}

	schedule, ok := a.ownedSchedule(w, r, serverID)
	if !ok {
		return
	}

	var req patchScheduleRequest
	if !decodeBody(w, r, &req) {
		return
	}

	if req.Name != nil {
		name, err := validateScheduleName(*req.Name)
		if err != nil {
			writeFieldError(w, "name", err.Error())
			return
		}
		schedule.Name = name
	}
	if req.Cron != nil {
		if err := validateCron(*req.Cron); err != nil {
			writeFieldError(w, "cron", err.Error())
			return
		}
		schedule.Cron = *req.Cron
	}
	if req.Actions != nil {
		if err := validateActions(*req.Actions); err != nil {
			writeFieldError(w, "actions", err.Error())
			return
		}
		if scope := missingScopeFor(principal, *req.Actions); scope != "" {
			writeError(w, http.StatusForbidden, CodeForbidden,
				"token is missing the "+scope+" scope, which this chain's actions require")
			return
		}
		schedule.Actions = *req.Actions
	}
	if req.Enabled != nil {
		// Switching a chain back on requires the scopes its actions need: a
		// token that could not have written the chain must not be able to
		// arm one somebody else left disabled.
		if *req.Enabled && !schedule.Enabled {
			if scope := missingScopeFor(principal, schedule.Actions); scope != "" {
				writeError(w, http.StatusForbidden, CodeForbidden,
					"token is missing the "+scope+" scope, which this chain's actions require")
				return
			}
		}
		schedule.Enabled = *req.Enabled
	}

	if err := a.store.Schedules.Update(r.Context(), schedule); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not update the schedule")
		return
	}

	a.audit(r, principal.UserID, "schedule.update", serverID, schedule.Name)
	writeJSON(w, http.StatusOK, toScheduleResponse(schedule))
}

// handleDeleteSchedule serves DELETE /servers/{id}/schedules/{sid}.
func (a *API) handleDeleteSchedule(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	principal, ok := a.authorizeServer(w, r, serverID, ScopeServersWrite)
	if !ok {
		return
	}

	schedule, ok := a.ownedSchedule(w, r, serverID)
	if !ok {
		return
	}
	if err := a.store.Schedules.Delete(r.Context(), schedule.ID); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not delete the schedule")
		return
	}

	a.audit(r, principal.UserID, "schedule.delete", serverID, schedule.Name)
	w.WriteHeader(http.StatusNoContent)
}

// handleRunSchedule serves POST /servers/{id}/schedules/{sid}/run.
//
// Running a chain by hand is how an operator checks it does what they meant
// without waiting for its cron to come round.
func (a *API) handleRunSchedule(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	principal, ok := a.authorizeServer(w, r, serverID, ScopeServersWrite)
	if !ok {
		return
	}

	schedule, ok := a.ownedSchedule(w, r, serverID)
	if !ok {
		return
	}
	if scope := missingScopeFor(principal, schedule.Actions); scope != "" {
		writeError(w, http.StatusForbidden, CodeForbidden,
			"token is missing the "+scope+" scope, which this chain's actions require")
		return
	}

	server, err := a.store.Servers.GetByID(r.Context(), serverID)
	if err != nil {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "server not found")
		return
	}

	task, started := a.startScheduleRun(r.Context(), schedule, server, time.Now().UTC())
	if !started {
		writeError(w, http.StatusConflict, "schedule_already_running",
			"this schedule is already running")
		return
	}

	a.audit(r, principal.UserID, "schedule.run", serverID, schedule.Name)
	writeJSON(w, http.StatusAccepted, taskAcceptedResponse{TaskID: task.ID})
}

// ownedSchedule loads the schedule named in the path and checks it belongs to
// the server in the path.
//
// The pairing matters: without it, knowing any schedule id would let a caller
// act on it through a server they do own.
func (a *API) ownedSchedule(w http.ResponseWriter, r *http.Request, serverID string) (*store.Schedule, bool) {
	schedule, err := a.store.Schedules.GetByID(r.Context(), r.PathValue("sid"))
	if err != nil || schedule.ServerID != serverID {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "schedule not found")
		return nil, false
	}
	return schedule, true
}

// --- running ---

// RunSchedules fires due chains until ctx is cancelled.
//
// It ticks rather than sleeping until the next due time, so a schedule added
// or changed while the daemon runs is picked up without restarting anything.
func (a *API) RunSchedules(ctx context.Context, interval time.Duration) {
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
			a.fireDueSchedules(ctx, now)
		}
	}
}

func (a *API) fireDueSchedules(ctx context.Context, now time.Time) {
	schedules, err := a.store.Schedules.ListEnabled(ctx)
	if err != nil {
		a.log.Warn("reading schedules failed", slog.String("error", err.Error()))
		return
	}

	for _, schedule := range schedules {
		parsed, err := cronParser.Parse(schedule.Cron)
		if err != nil {
			a.log.Warn("a stored cron expression is invalid",
				slog.String("schedule_id", schedule.ID), slog.String("cron", schedule.Cron))
			continue
		}

		// Due when the next run after the last one has already passed. Using
		// the recorded last run rather than a timer means a daemon that was
		// down over a scheduled time runs the chain once on the next tick,
		// rather than skipping the day entirely.
		from := schedule.CreatedAt
		if schedule.LastRunAt != nil {
			from = *schedule.LastRunAt
		}
		if parsed.Next(from).After(now) {
			continue
		}

		server, err := a.store.Servers.GetByID(ctx, schedule.ServerID)
		if err != nil {
			continue
		}
		a.startScheduleRun(ctx, schedule, server, now)
	}
}

// startScheduleRun begins a chain unless it is already running.
func (a *API) startScheduleRun(ctx context.Context, schedule *store.Schedule, server *store.Server, now time.Time) (*Task, bool) {
	// A chain that waits can outlast the tick interval, so an in-memory claim
	// guards the overlap the recorded run time cannot.
	if !a.scheduleRuns.claim(schedule.ID) {
		return nil, false
	}

	if err := a.store.Schedules.MarkRun(ctx, schedule.ID, now); err != nil {
		a.log.Warn("recording the schedule run failed", slog.String("error", err.Error()))
		a.scheduleRuns.release(schedule.ID)
		return nil, false
	}

	a.log.Info("schedule starting",
		slog.String("schedule_id", schedule.ID), slog.String("name", schedule.Name),
		slog.String("server_id", schedule.ServerID))

	task := a.tasks.start("schedule.run", schedule.ServerID, server.OwnerID,
		func(taskCtx context.Context) error {
			defer a.scheduleRuns.release(schedule.ID)

			err := a.runChain(taskCtx, schedule, server)

			status, message := store.ScheduleRunOK, ""
			if err != nil {
				status, message = store.ScheduleRunFailed, err.Error()
			}
			// context.WithoutCancel: the outcome is worth recording even when
			// the chain failed because the task's deadline passed.
			if recErr := a.store.Schedules.RecordOutcome(
				context.WithoutCancel(taskCtx), schedule.ID, status, message); recErr != nil {
				a.log.Warn("recording the schedule outcome failed",
					slog.String("error", recErr.Error()))
			}
			return err
		})

	return task, true
}

// runChain performs a schedule's actions in order, stopping at the first
// failure.
//
// Stopping rather than carrying on is the safer reading of a chain: "warn the
// players, wait, stop the server" that failed to warn should not go on to stop
// it anyway.
func (a *API) runChain(ctx context.Context, schedule *store.Schedule, server *store.Server) error {
	for i, action := range schedule.Actions {
		if err := a.runAction(ctx, action, server); err != nil {
			return fmt.Errorf("action %d (%s): %w", i+1, action.Type, err)
		}
	}
	return nil
}

func (a *API) runAction(ctx context.Context, action store.Action, server *store.Server) error {
	switch action.Type {
	case store.ActionCommand:
		if a.console == nil {
			return errors.New("no runner is configured on this node")
		}
		return a.console.SendCommand(ctx, server.ID, action.String("command"))

	case store.ActionPower:
		if a.lifecycle == nil {
			return errors.New("no runner is configured on this node")
		}
		power := action.String("action")
		// Starting a running server, or stopping a stopped one, is what the
		// operator meant to end up with either way, so it is not an error that
		// aborts the rest of the chain.
		status, _ := a.serverStatus(ctx, server.ID)
		if (power == ActionStart && status.IsActive()) ||
			((power == ActionStop || power == ActionKill) && !status.IsActive()) {
			return nil
		}
		return a.runPower(ctx, server, power, a.stopTimeout)

	case store.ActionBackup:
		if a.backups == nil {
			return errors.New("backups are not configured on this node")
		}
		record := &store.Backup{
			ServerID: server.ID, Note: action.String("note"), State: store.BackupPending,
		}
		if err := a.store.Backups.Create(ctx, record); err != nil {
			return err
		}
		return a.runBackup(ctx, server, record)

	case store.ActionWait:
		seconds, _ := action.Int("seconds")
		timer := time.NewTimer(time.Duration(seconds) * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
	return fmt.Errorf("unknown action type %q", action.Type)
}

// runningSchedules tracks chains in flight, so one cannot overlap itself.
type runningSchedules struct {
	mu  sync.Mutex
	ids map[string]struct{}
}

func newRunningSchedules() *runningSchedules {
	return &runningSchedules{ids: make(map[string]struct{})}
}

// claim reports whether the caller may start this schedule.
func (r *runningSchedules) claim(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, running := r.ids[id]; running {
		return false
	}
	r.ids[id] = struct{}{}
	return true
}

func (r *runningSchedules) release(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.ids, id)
}
