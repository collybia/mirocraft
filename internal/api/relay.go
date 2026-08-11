package api

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/collybia/mirocraft/internal/config"
	"github.com/collybia/mirocraft/internal/relay"
	"github.com/collybia/mirocraft/internal/store"
)

// RelayMarker names the file that records which server holds the tunnel.
//
// A file beside the server rather than a column: the tunnel is a property of
// this installation's relationship with one relay, not of the server, and a
// marker that travels with the directory survives everything a database
// migration would have to be written for.
const RelayMarker = ".mirocraft-relay"

// relayInfo is what the panel says about the tunnel.
type relayInfo struct {
	// Configured reports whether this panel knows of a relay at all.
	Configured bool `json:"configured"`
	// Enabled reports whether this server is the one using it.
	Enabled bool `json:"enabled"`
	// Address is what to hand out, once the tunnel is up.
	Address string `json:"address,omitempty"`
	// HeldBy names the server currently using the tunnel, when it is not this
	// one. A tunnel forwards one public port, so it belongs to one server.
	HeldBy string `json:"held_by,omitempty"`
	// Error is why the tunnel is not up, in words rather than a code.
	Error string `json:"error,omitempty"`
}

// relayManager keeps at most one tunnel running.
//
// One, because a relay tunnel is one public port: two servers behind the same
// tunnel would mean players of one arriving at the other. Which server holds
// it is the operator's choice, and switching is explicit.
type relayManager struct {
	cfg     config.RelayConfig
	dataDir string
	log     *slog.Logger

	mu       sync.Mutex
	serverID string
	agent    *relay.Agent
	cancel   context.CancelFunc
	lastErr  string
}

func newRelayManager(cfg config.RelayConfig, dataDir string, log *slog.Logger) *relayManager {
	return &relayManager{cfg: cfg, dataDir: dataDir, log: log}
}

// Configured reports whether a relay is available at all.
func (m *relayManager) Configured() bool {
	return m != nil && m.cfg.Enabled()
}

// Start brings the tunnel up for a server, replacing whatever held it.
func (m *relayManager) Start(serverID string, port int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.startLocked(serverID, port)
}

func (m *relayManager) startLocked(serverID string, port int) {
	if m.cancel != nil {
		m.cancel()
	}

	ctx, cancel := context.WithCancel(context.Background())
	agent := relay.NewAgent(relay.AgentConfig{
		Addr:        m.cfg.Addr,
		Token:       m.cfg.Token,
		Target:      net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		Insecure:    m.cfg.Insecure,
		Fingerprint: m.cfg.Fingerprint,
		Log:         m.log,
	})

	m.serverID = serverID
	m.agent = agent
	m.cancel = cancel
	m.lastErr = ""

	go func() {
		if err := agent.Run(ctx); err != nil && ctx.Err() == nil {
			m.mu.Lock()
			m.lastErr = err.Error()
			m.mu.Unlock()
		}
	}()
}

// Stop takes the tunnel down.
func (m *relayManager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cancel != nil {
		m.cancel()
	}
	m.serverID, m.agent, m.cancel = "", nil, nil
}

// Info describes the tunnel from one server's point of view.
func (m *relayManager) Info(ctx context.Context, serverID string) relayInfo {
	if !m.Configured() {
		return relayInfo{}
	}

	m.mu.Lock()
	holder, agent, lastErr := m.serverID, m.agent, m.lastErr
	m.mu.Unlock()

	info := relayInfo{Configured: true}
	if holder == "" {
		return info
	}
	if holder != serverID {
		info.HeldBy = holder
		return info
	}

	info.Enabled = true
	info.Error = lastErr

	// A short wait rather than none: the port is known the moment the relay
	// answers, and a page that says "connecting…" for a tunnel that is
	// already up is a page people reload.
	wait, stop := context.WithTimeout(ctx, 2*time.Second)
	defer stop()

	if port := agent.Port(wait); port > 0 {
		info.Address = net.JoinHostPort(m.publicHost(), strconv.Itoa(port))
	}
	return info
}

// publicHost is the name players type. The control address is the panel's own
// business and may not be where players connect.
func (m *relayManager) publicHost() string {
	if m.cfg.Host != "" {
		return m.cfg.Host
	}
	if host, _, err := net.SplitHostPort(m.cfg.Addr); err == nil {
		return host
	}
	return m.cfg.Addr
}

// --- persistence ---

// markerPath is where a server records that it holds the tunnel.
func (a *API) relayMarkerPath(server *store.Server) string {
	return filepath.Join(a.serverDir(server), RelayMarker)
}

// RestoreRelay brings the tunnel back after a restart.
//
// Without this the tunnel would be up until the first upgrade and then quietly
// not, which is the worst of both: the address a friend saved keeps looking
// right and stops working.
func (a *API) RestoreRelay(ctx context.Context) {
	if !a.relay.Configured() {
		return
	}

	servers, err := a.store.Servers.List(ctx, store.ServerFilter{})
	if err != nil {
		return
	}
	for _, server := range servers {
		if _, err := os.Stat(a.relayMarkerPath(server)); err != nil {
			continue
		}
		a.relay.Start(server.ID, server.Port)
		a.log.Info("relay tunnel restored", slog.String("server_id", server.ID))
		return
	}
}

// --- handlers ---

// handleRelayEnable serves POST /servers/{id}/connect/relay.
func (a *API) handleRelayEnable(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	principal, ok := a.authorizeServer(w, r, serverID, ScopeServersWrite)
	if !ok {
		return
	}

	if !a.relay.Configured() {
		writeError(w, http.StatusConflict, CodeValidationFailed,
			"no relay is configured for this panel")
		return
	}

	server, err := a.store.Servers.GetByID(r.Context(), serverID)
	if err != nil {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "server not found")
		return
	}

	// Whoever held it before loses it, and the marker goes with it: two
	// markers would mean a restart picking whichever it read first.
	a.clearRelayMarkers(r.Context())

	// #nosec G306 -- a marker, not a secret: its presence is the whole content
	if err := os.WriteFile(a.relayMarkerPath(server), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not record the tunnel")
		return
	}

	a.relay.Start(server.ID, server.Port)
	a.audit(r, principal.UserID, "relay.enable", server.ID, "")

	writeJSON(w, http.StatusOK, a.connectInfo(r.Context(), server))
}

// handleRelayDisable serves DELETE /servers/{id}/connect/relay.
func (a *API) handleRelayDisable(w http.ResponseWriter, r *http.Request) {
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

	// #nosec G703 -- a missing marker is the state being asked for
	_ = os.Remove(a.relayMarkerPath(server))
	a.relay.Stop()
	a.audit(r, principal.UserID, "relay.disable", server.ID, "")

	w.WriteHeader(http.StatusNoContent)
}

// clearRelayMarkers removes every marker, so exactly one can be written next.
func (a *API) clearRelayMarkers(ctx context.Context) {
	servers, err := a.store.Servers.List(ctx, store.ServerFilter{})
	if err != nil {
		return
	}
	for _, server := range servers {
		if err := os.Remove(a.relayMarkerPath(server)); err != nil && !errors.Is(err, os.ErrNotExist) {
			a.log.Debug("removing a relay marker failed",
				slog.String("server_id", server.ID), slog.String("error", err.Error()))
		}
	}
}
