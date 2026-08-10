package api

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/collybia/mirocraft/internal/runner"
	"github.com/collybia/mirocraft/internal/store"
)

// Auto-restart limits.
const (
	// autoRestartDelay is the pause before bringing a crashed server back.
	//
	// Not zero: the port takes a moment to be released, and a restart that
	// races that fails with "address already in use" — which looks like a
	// second, different fault.
	autoRestartDelay = 5 * time.Second

	// autoRestartAttempts is how many times a server is brought back inside
	// one window before the panel stops trying.
	autoRestartAttempts = 3

	// autoRestartWindow is how long those attempts are counted for. A server
	// that has run cleanly for longer than this starts again from zero: a
	// crash today has nothing to do with one last week.
	autoRestartWindow = 10 * time.Minute
)

// autoRestarter counts recent restarts per server, so a server that crashes on
// startup is not restarted forever.
//
// A crash loop is worse than a stopped server: it fills the disk with logs,
// keeps the port flapping, and hides the original fault under a thousand
// repetitions of it. Three tries in ten minutes, then the panel leaves it
// alone and says so.
type autoRestarter struct {
	mu    sync.Mutex
	now   func() time.Time
	tries map[string]restartWindow
}

type restartWindow struct {
	count int
	first time.Time
}

func newAutoRestarter() *autoRestarter {
	return &autoRestarter{now: time.Now, tries: make(map[string]restartWindow)}
}

// allow records an attempt and reports whether it should go ahead.
func (r *autoRestarter) allow(serverID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	window := r.tries[serverID]
	if window.count == 0 || now.Sub(window.first) > autoRestartWindow {
		r.tries[serverID] = restartWindow{count: 1, first: now}
		return true
	}
	if window.count >= autoRestartAttempts {
		return false
	}
	window.count++
	r.tries[serverID] = window
	return true
}

// forget clears a server's history, for an operator starting it by hand: that
// is a decision, and it should not be spent against the crash budget.
func (r *autoRestarter) forget(serverID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tries, serverID)
}

// StartAutoStartServers starts the servers marked to come up with the daemon.
//
// A panel on a rented machine is restarted by things nobody chose — a kernel
// update, a reboot from the hoster's console, a power event. Without this the
// worlds stay down until somebody notices and presses start, and the switch in
// the panel that says otherwise is decoration.
//
// Sequential on purpose. Starting four servers at once on a small box means
// four JVMs claiming their heaps in the same second, and the machine that
// would have run them one after another kills one of them instead.
func (a *API) StartAutoStartServers(ctx context.Context) {
	servers, err := a.store.Servers.List(ctx, store.ServerFilter{})
	if err != nil {
		a.log.Warn("reading servers for auto-start failed", slog.String("error", err.Error()))
		return
	}

	for _, server := range servers {
		if !server.AutoStart {
			continue
		}
		if status, err := a.serverStatus(ctx, server.ID); err == nil && status.IsActive() {
			continue
		}

		a.log.Info("starting a server marked auto-start",
			slog.String("server_id", server.ID), slog.String("name", server.Name))

		if err := a.startServer(ctx, server); err != nil {
			// Logged and moved past: one server that cannot start is not a
			// reason to leave the rest down.
			a.log.Warn("auto-start failed",
				slog.String("server_id", server.ID), slog.String("name", server.Name),
				slog.String("error", err.Error()))
			continue
		}
		a.log.Info("auto-started", slog.String("server_id", server.ID))
	}
}

// autoRestart brings a crashed server back, if its owner asked for that.
//
// Only a crash. A server the operator stopped stays stopped — restarting it
// would make the stop button a suggestion.
func (a *API) autoRestart(serverID string) {
	server, err := a.store.Servers.GetByID(context.Background(), serverID)
	if err != nil || !server.AutoRestart {
		return
	}

	if !a.restarts.allow(serverID) {
		a.log.Warn("a server keeps crashing, so the panel has stopped restarting it; "+
			"the console holds the reason",
			slog.String("server_id", serverID), slog.String("name", server.Name),
			slog.Int("attempts", autoRestartAttempts), slog.Duration("within", autoRestartWindow))
		return
	}

	a.log.Info("restarting a crashed server",
		slog.String("server_id", serverID), slog.String("name", server.Name))

	// Detached from the caller: this runs inside the status watcher, and
	// blocking it would stop the console and the event stream for as long as
	// provisioning takes.
	go func() {
		time.Sleep(autoRestartDelay)

		ctx, cancel := context.WithTimeout(context.Background(), TaskTimeout)
		defer cancel()

		if err := a.startServer(ctx, server); err != nil {
			a.log.Warn("restarting a crashed server failed",
				slog.String("server_id", serverID), slog.String("error", err.Error()))
			return
		}
		a.log.Info("restarted after a crash", slog.String("server_id", serverID))
	}()
}

// crashed reports whether a status is the kind auto-restart exists for.
func crashed(status runner.Status) bool { return status == runner.StatusCrashed }
