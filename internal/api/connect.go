package api

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/collybia/mirocraft/internal/netinfo"
	"github.com/collybia/mirocraft/internal/store"
	"github.com/collybia/mirocraft/internal/upnp"
)

// routerCacheTTL is how long a discovered router is reused.
//
// Discovery is a multicast search with a three-second wait for the routers
// that are not there, and this endpoint is opened every time somebody looks at
// a server. A router does not appear and disappear on that timescale.
const routerCacheTTL = 5 * time.Minute

// upnpTimeout bounds one conversation with the router.
const upnpTimeout = 20 * time.Second

// routerCache remembers the gateway between requests.
type routerCache struct {
	mu       sync.Mutex
	router   *upnp.Router
	err      error
	foundAt  time.Time
	now      func() time.Time
	discover func(context.Context) (*upnp.Router, error)
}

func newRouterCache() *routerCache {
	return &routerCache{now: time.Now, discover: upnp.Discover}
}

func (c *routerCache) get(ctx context.Context) (*upnp.Router, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.now().Sub(c.foundAt) < routerCacheTTL && (c.router != nil || c.err != nil) {
		return c.router, c.err
	}
	c.router, c.err = c.discover(ctx)
	c.foundAt = c.now()
	return c.router, c.err
}

// forget drops the cached router, for after a change that its answers depend
// on.
func (c *routerCache) forget() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.foundAt = time.Time{}
}

// --- wire types ---

// internetState says whether players outside this network can reach the
// server, and when they cannot, why.
type internetState string

const (
	// stateDirect: the machine holds a public address, so the port only has to
	// be open. A rented VPS is this.
	stateDirect internetState = "direct"
	// stateForwarded: behind a router, and the router sends this port here.
	stateForwarded internetState = "forwarded"
	// stateCanForward: behind a router that could do it and has not been asked.
	stateCanForward internetState = "can_forward"
	// stateTakenByAnother: the router already sends this port to a different
	// machine — a console, another PC — and taking it would break that.
	stateTakenByAnother internetState = "taken_by_another"
	// stateCarrierNAT: the provider hands out a shared address, so no amount
	// of router configuration will help. This is where an overlay network is
	// the answer rather than a workaround.
	stateCarrierNAT internetState = "carrier_nat"
	// stateNoRouter: nothing answered the UPnP search.
	stateNoRouter internetState = "no_router"
)

type connectAddress struct {
	Address string       `json:"address"`
	IP      string       `json:"ip"`
	Kind    netinfo.Kind `json:"kind"`
	// Interface is the adapter's name, so somebody running two overlay
	// networks can tell which line is which.
	Interface string `json:"interface"`
}

type internetInfo struct {
	State internetState `json:"state"`
	// ExternalIP is what the internet sees, when the router will say.
	ExternalIP string `json:"external_ip,omitempty"`
	// Address is what to hand out, when there is something to hand out.
	Address string `json:"address,omitempty"`
	// TakenBy names the machine already holding the port, for the one state
	// where the panel refuses to act on its own.
	TakenBy string `json:"taken_by,omitempty"`
}

type connectResponse struct {
	Port int `json:"port"`
	// Addresses are every way to reach this machine, most useful first.
	Addresses []connectAddress `json:"addresses"`
	Internet  internetInfo     `json:"internet"`
}

// --- handlers ---

// handleConnect serves GET /servers/{id}/connect.
//
// The question a panel on somebody's own machine is actually asked: what
// address do I give my friends? A machine hosting for friends has four
// addresses and nothing to say which is which, so people hand out localhost
// and spend an evening wondering.
func (a *API) handleConnect(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	if _, ok := a.authorizeServer(w, r, serverID, ScopeServersRead); !ok {
		return
	}

	server, err := a.store.Servers.GetByID(r.Context(), serverID)
	if err != nil {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "server not found")
		return
	}

	writeJSON(w, http.StatusOK, a.connectInfo(r.Context(), server))
}

// connectInfo gathers the addresses and asks the router where it stands.
func (a *API) connectInfo(ctx context.Context, server *store.Server) connectResponse {
	resp := connectResponse{Port: server.Port, Addresses: []connectAddress{}}

	// Through a field rather than the package directly, so a test can say what
	// kind of machine this is instead of depending on the one it runs on.
	list := a.addresses
	if list == nil {
		list = netinfo.Addresses
	}
	addrs, err := list()
	if err != nil {
		a.log.Warn("listing local addresses failed", slog.String("error", err.Error()))
	}
	for _, addr := range addrs {
		resp.Addresses = append(resp.Addresses, connectAddress{
			Address:   joinHostPort(addr.IP, server.Port),
			IP:        addr.IP,
			Kind:      addr.Kind,
			Interface: addr.Interface,
		})
	}

	// A machine with its own public address needs no router asked: it is the
	// far end already.
	if netinfo.HasPublic(addrs) {
		resp.Internet = internetInfo{State: stateDirect}
		for _, addr := range addrs {
			if addr.Kind == netinfo.KindPublic {
				resp.Internet.ExternalIP = addr.IP
				resp.Internet.Address = joinHostPort(addr.IP, server.Port)
				break
			}
		}
		return resp
	}

	resp.Internet = a.askRouter(ctx, server)
	return resp
}

// askRouter works out what the gateway can do for this server.
func (a *API) askRouter(ctx context.Context, server *store.Server) internetInfo {
	if a.routers == nil {
		return internetInfo{State: stateNoRouter}
	}

	ctx, cancel := context.WithTimeout(ctx, upnpTimeout)
	defer cancel()

	router, err := a.routers.get(ctx)
	if err != nil {
		return internetInfo{State: stateNoRouter}
	}

	info := internetInfo{State: stateCanForward}
	if external, err := router.ExternalIP(ctx); err == nil {
		info.ExternalIP = external
		// A router whose own uplink address is private or carrier-grade is
		// behind another layer of NAT, and forwarding a port through it
		// reaches nobody. Saying so is the whole value of asking.
		if isSharedAddress(external) {
			return internetInfo{State: stateCarrierNAT, ExternalIP: external}
		}
		info.Address = joinHostPort(external, server.Port)
	}

	mapping, found, err := router.Lookup(ctx, server.Port, false)
	switch {
	case err != nil:
		// The router is there and would not answer; treat it as askable
		// rather than as broken.
	case found && mapping.InternalClient == router.LocalIP:
		info.State = stateForwarded
	case found:
		info.State = stateTakenByAnother
		info.TakenBy = mapping.InternalClient
	}
	return info
}

// handleForward serves POST /servers/{id}/connect/forward.
//
// A button rather than something the panel does at startup. A firewall rule
// changes the machine somebody installed the panel on; a port forwarding
// changes the router the whole household shares, and that is not a decision to
// make on their behalf.
func (a *API) handleForward(w http.ResponseWriter, r *http.Request) {
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
	if a.routers == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternalError,
			"port forwarding is switched off on this node")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), upnpTimeout)
	defer cancel()

	router, err := a.routers.get(ctx)
	if err != nil {
		writeError(w, http.StatusNotFound, "no_router",
			"no router answered; either it does not speak UPnP or the feature is switched off in it")
		return
	}

	// Somebody else's mapping is left alone: taking it would silently break
	// whatever was using it, and the panel has no way to know what that is.
	if mapping, found, _ := router.Lookup(ctx, server.Port, false); found &&
		mapping.InternalClient != router.LocalIP {
		writeErrorDetails(w, http.StatusConflict, "port_in_use",
			"the router already sends this port to another machine",
			map[string]any{"taken_by": mapping.InternalClient})
		return
	}

	description := "Mirocraft " + server.Name
	if err := router.Forward(ctx, server.Port, false, description); err != nil {
		a.log.Warn("the router refused to forward a port",
			slog.String("server_id", server.ID), slog.Int("port", server.Port),
			slog.String("error", err.Error()))
		writeError(w, http.StatusBadGateway, "no_router", "the router refused: "+err.Error())
		return
	}

	a.log.Info("port forwarded on the router",
		slog.String("server_id", server.ID), slog.Int("port", server.Port))
	a.audit(r, principal.UserID, "server.forward", server.ID, "")

	a.routers.forget()
	writeJSON(w, http.StatusOK, a.connectInfo(r.Context(), server))
}

// handleUnforward serves DELETE /servers/{id}/connect/forward.
func (a *API) handleUnforward(w http.ResponseWriter, r *http.Request) {
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
	if a.routers == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), upnpTimeout)
	defer cancel()

	router, err := a.routers.get(ctx)
	if err != nil {
		// Nothing to close is the outcome asked for.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Only a mapping pointing here is removed: one somebody else made is
	// theirs.
	if mapping, found, _ := router.Lookup(ctx, server.Port, false); found &&
		mapping.InternalClient != router.LocalIP {
		writeErrorDetails(w, http.StatusConflict, "port_in_use",
			"this port is forwarded to another machine, so it is not the panel's to remove",
			map[string]any{"taken_by": mapping.InternalClient})
		return
	}

	if err := router.Remove(ctx, server.Port, false); err != nil && !errors.Is(err, upnp.ErrRefused) {
		writeError(w, http.StatusBadGateway, "no_router", "the router refused: "+err.Error())
		return
	}

	a.log.Info("port forwarding removed",
		slog.String("server_id", server.ID), slog.Int("port", server.Port))
	a.audit(r, principal.UserID, "server.unforward", server.ID, "")

	a.routers.forget()
	w.WriteHeader(http.StatusNoContent)
}

// joinHostPort formats an address a person can type into a game.
func joinHostPort(ip string, port int) string {
	return net.JoinHostPort(ip, strconv.Itoa(port))
}

func parseIP(ip string) net.IP { return net.ParseIP(ip) }

// isSharedAddress reports whether an address cannot be reached from the
// internet: a private range, or the carrier-grade NAT block providers use when
// they have run out.
func isSharedAddress(ip string) bool {
	parsed := parseIP(ip)
	if parsed == nil {
		return false
	}
	if parsed.IsPrivate() || parsed.IsLoopback() || parsed.IsUnspecified() {
		return true
	}
	four := parsed.To4()
	return four != nil && four[0] == 100 && four[1] >= 64 && four[1] <= 127
}
