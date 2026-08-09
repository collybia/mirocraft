package api

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/collybia/mirocraft/internal/dns"
	"github.com/collybia/mirocraft/internal/store"
)

// DNSPublisher publishes the records a server needs to be reachable by name.
//
// An interface rather than the concrete provider so the API can be built
// without one — a panel reached by IP address is a supported install, not a
// degraded one.
type DNSPublisher interface {
	Zone() string
	Capabilities() dns.Capabilities
	EnsureSRV(ctx context.Context, sub, target string, port int) error
	Cleanup(ctx context.Context, sub string) error
}

// DNSStatus is what the panel shows about the published name.
type dnsStatusResponse struct {
	// Enabled is false when the daemon publishes no records at all.
	Enabled bool `json:"enabled"`
	// Provider and Zone identify where records are written.
	Provider string `json:"provider,omitempty"`
	Zone     string `json:"zone,omitempty"`
	// SRV reports whether servers on non-standard ports can be reached by
	// name. False means players have to type the port.
	SRV bool `json:"srv"`

	// Host is the name the panel's own address is published as, with the
	// address and any failure from the last check.
	Host *dns.Status `json:"host,omitempty"`
}

// handleDNSStatus serves GET /dns.
//
// The panel needs this to tell an operator what address to give players, and
// to say why a name stopped resolving: a watcher that has been failing for a
// day looks exactly like a server that is down.
func (a *API) handleDNSStatus(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireScope(w, r, ScopeServersRead); !ok {
		return
	}

	if a.dns == nil {
		writeJSON(w, http.StatusOK, dnsStatusResponse{Enabled: false})
		return
	}

	resp := dnsStatusResponse{
		Enabled: true,
		Zone:    a.dns.Zone(),
		SRV:     a.dns.Capabilities().SRV,
	}
	if provider, ok := a.dns.(interface{ ID() string }); ok {
		resp.Provider = provider.ID()
	}
	if a.dnsWatcher != nil {
		status := a.dnsWatcher.Status()
		resp.Host = &status
	}

	writeJSON(w, http.StatusOK, resp)
}

// serverAddress is what players type to join.
type serverAddress struct {
	// Host is the name or address to connect to.
	Host string `json:"host"`
	// Port is what players must add when the name alone is not enough.
	Port int `json:"port"`
	// NeedsPort reports whether players have to type the port. True whenever
	// there is no SRV record — on DuckDNS always, and everywhere for a server
	// on the default port where SRV is unnecessary anyway.
	NeedsPort bool `json:"needs_port"`
}

// serverSub is the name a server is published under.
//
// Derived from the server's name rather than its id: "survival.example.com"
// is something a player can be told, and "01KZH27YV0.example.com" is not.
// Collisions fall back to the id, because two servers sharing a name would
// otherwise overwrite each other's records.
func (a *API) serverSub(ctx context.Context, server *store.Server) string {
	slug := slugify(server.Name)
	if slug == "" {
		return strings.ToLower(server.ID)
	}

	// A name that another server already holds is not usable: whichever
	// published last would win, and the other server would quietly become
	// unreachable.
	others, err := a.store.Servers.List(ctx, store.ServerFilter{})
	if err == nil {
		for _, other := range others {
			if other.ID != server.ID && slugify(other.Name) == slug {
				return slug + "-" + strings.ToLower(server.ID[len(server.ID)-6:])
			}
		}
	}
	return slug
}

var nonHostname = regexp.MustCompile(`[^a-z0-9-]+`)

// slugify turns a server name into a hostname label.
//
// Returns empty where nothing usable survives — a server named entirely in
// Cyrillic, which is normal here — and the caller falls back to the id rather
// than publishing a record under a mangled name.
func slugify(name string) string {
	lower := strings.ToLower(strings.TrimSpace(name))
	slug := nonHostname.ReplaceAllString(lower, "-")
	slug = strings.Trim(slug, "-")

	for strings.Contains(slug, "--") {
		slug = strings.ReplaceAll(slug, "--", "-")
	}
	if len(slug) > 63 {
		slug = strings.Trim(slug[:63], "-")
	}
	if dns.ValidateSub(slug) != nil {
		return ""
	}
	return slug
}

// publishServerDNS gives a server a name players can use.
//
// Best effort by design: a DNS provider being down must not stop a server
// being created, and a provider that cannot publish SRV is not a failure —
// it is a fact the panel reports so players are told to include the port.
func (a *API) publishServerDNS(ctx context.Context, server *store.Server) {
	if a.dns == nil {
		return
	}

	sub := a.serverSub(ctx, server)
	if err := dns.EnsureServerSRV(ctx, a.dns, sub, server.Port); err != nil {
		if dns.IsUnsupported(err) {
			// Not worth a warning: it is a property of the provider the
			// operator chose, reported through the status endpoint instead.
			a.log.Debug("the DNS provider cannot publish SRV",
				slog.String("server_id", server.ID))
			return
		}
		a.log.Warn("publishing the server's SRV record failed",
			slog.String("server_id", server.ID), slog.String("error", err.Error()))
		return
	}

	a.log.Info("server SRV record published",
		slog.String("server_id", server.ID),
		slog.String("name", dns.FQDN(sub, a.dns.Zone())),
		slog.Int("port", server.Port))
}

// unpublishServerDNS removes a deleted server's records.
func (a *API) unpublishServerDNS(ctx context.Context, server *store.Server) {
	if a.dns == nil {
		return
	}

	sub := a.serverSub(ctx, server)
	if err := a.dns.Cleanup(ctx, sub); err != nil {
		// Worth saying: a leftover SRV points players at a port where nothing
		// is listening, which is more confusing than no record at all.
		a.log.Warn("removing the server's DNS records failed",
			slog.String("server_id", server.ID), slog.String("error", err.Error()))
	}
}

// AddressFor reports how players reach a server.
func (a *API) addressFor(ctx context.Context, server *store.Server) serverAddress {
	if a.dns == nil {
		return serverAddress{Port: server.Port, NeedsPort: true}
	}

	sub := a.serverSub(ctx, server)
	// Without SRV the name resolves to the host and nothing carries the port,
	// so players must type it.
	return serverAddress{
		Host:      dns.FQDN(sub, a.dns.Zone()),
		Port:      server.Port,
		NeedsPort: !a.dns.Capabilities().SRV,
	}
}
