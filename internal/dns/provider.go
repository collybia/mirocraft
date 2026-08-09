// Package dns publishes the records a Minecraft server needs to be reachable
// by name: an address record for the host, and an SRV record so players can
// connect without typing a port.
//
// Three providers, because they answer three different situations. deSEC and
// DuckDNS hand out a free subdomain to someone who owns no domain at all;
// Cloudflare drives a domain the operator already has. They are not
// interchangeable — DuckDNS cannot publish SRV, and a panel that pretended
// otherwise would produce a server nobody can reach on a non-standard port.
package dns

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
)

// Errors callers distinguish.
var (
	// ErrUnsupported reports an operation this provider cannot perform, such
	// as SRV on DuckDNS. Distinguishable so the caller can degrade rather
	// than fail: a server on the default port needs no SRV.
	ErrUnsupported = errors.New("dns: the provider does not support this")
	// ErrAuth reports credentials the provider refused.
	ErrAuth = errors.New("dns: the provider rejected the credentials")
	// ErrNotFound reports a zone or record the provider does not know.
	ErrNotFound = errors.New("dns: not found")
)

// MinecraftSRVPrefix is the name a Java client looks up before connecting.
//
// Only Java. Bedrock clients do not read SRV at all, which is why the panel
// tries to give Bedrock servers the standard 19132 instead.
const MinecraftSRVPrefix = "_minecraft._tcp"

// DefaultTTL is short enough that a changed address propagates within
// minutes, which is what a home connection with a moving IP needs.
//
// Providers that impose a floor raise it: deSEC's free tier does not accept
// anything under an hour, and asking for less is rejected rather than
// silently rounded.
const DefaultTTL = 300

// Provider publishes records in one zone.
//
// Bound to its zone at construction rather than taking one per call: every
// implementation needs zone-specific setup — Cloudflare a zone id, deSEC a
// domain, DuckDNS the single name it was registered for — and threading that
// through each call would put provider details in the caller.
type Provider interface {
	// ID is the stable identifier used in configuration.
	ID() string
	// Name is what the panel and the installer show.
	Name() string
	// Zone is the domain this provider writes into.
	Zone() string

	// EnsureAddress publishes an A or AAAA record, choosing by the address
	// family. sub is the name relative to the zone; empty means the zone
	// itself.
	//
	// Named for what it does rather than for the record type: a v6-only host
	// needs AAAA, and a method called EnsureA that sometimes writes AAAA is a
	// small lie that costs someone an afternoon.
	EnsureAddress(ctx context.Context, sub string, ip netip.Addr) error

	// EnsureSRV publishes _minecraft._tcp.<sub> pointing at target:port.
	// Returns ErrUnsupported where the provider cannot.
	EnsureSRV(ctx context.Context, sub, target string, port int) error

	// Capabilities reports what this provider can do, so a caller can ask
	// before trying rather than discovering it from an error.
	Capabilities() Capabilities

	// Cleanup removes every record this panel created for sub.
	Cleanup(ctx context.Context, sub string) error
}

// Capabilities describes a provider's limits.
type Capabilities struct {
	// SRV reports whether SRV records can be published.
	SRV bool
	// Subdomains reports whether names other than the registered one can be
	// used. DuckDNS gives one name and nothing under it.
	Subdomains bool
	// MinTTL is the shortest TTL the provider accepts. Zero means no floor.
	MinTTL int
}

// subPattern is what may appear as a subdomain.
//
// Checked here rather than left to the provider because the value reaches an
// upstream URL and a record name, and the panel should not be in the business
// of finding out what a given API does with a semicolon.
var subPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidateSub checks a subdomain label.
func ValidateSub(sub string) error {
	if sub == "" {
		return nil // the zone itself
	}
	lower := strings.ToLower(sub)
	if lower != sub {
		return fmt.Errorf("dns: %q must be lowercase", sub)
	}
	for _, label := range strings.Split(sub, ".") {
		if !subPattern.MatchString(label) {
			return fmt.Errorf("dns: %q is not a valid hostname label", label)
		}
	}
	return nil
}

// FQDN joins a subdomain and a zone into a full name.
func FQDN(sub, zone string) string {
	if sub == "" {
		return zone
	}
	return sub + "." + zone
}

// RecordType returns A or AAAA for an address.
func RecordType(ip netip.Addr) string {
	if ip.Is4() {
		return "A"
	}
	return "AAAA"
}

// SRVValue formats an SRV record's data.
//
// The trailing dot matters: without it a resolver appends the zone and the
// target becomes mc.example.com.example.com, which resolves to nothing and
// looks like a server that is simply down.
func SRVValue(target string, port int) string {
	return fmt.Sprintf("0 5 %d %s", port, strings.TrimSuffix(target, ".")+".")
}

// Registry holds the providers the daemon knows how to build.
type Registry struct {
	factories map[string]Factory
	order     []string
}

// Factory builds a provider from its configuration.
type Factory struct {
	// Name is what the installer shows in its menu.
	Name string
	// Description says when to pick this one.
	Description string
	// NeedsZone reports whether the operator supplies a domain. DuckDNS and
	// deSEC subdomains are chosen by the operator too, but Cloudflare needs
	// one they already own.
	NeedsZone bool
	// New builds the provider.
	New func(cfg Config) (Provider, error)
}

// Config is what every provider needs from the operator.
type Config struct {
	// Zone is the domain, for example "example.com" or "myname.duckdns.org".
	Zone string
	// Token authenticates with the provider.
	Token string
	// TTL overrides DefaultTTL. Providers raise it to their own floor.
	TTL int
}

// NewRegistry returns a registry with the implemented providers.
func NewRegistry() *Registry {
	r := &Registry{factories: make(map[string]Factory)}

	r.Register("desec", Factory{
		Name: "deSEC",
		Description: "Бесплатный поддомен вида имя.dedyn.io. Умеет SRV, " +
			"поэтому сервер на нестандартном порту находится по имени.",
		New: func(cfg Config) (Provider, error) { return NewDeSEC(cfg, nil) },
	})
	r.Register("duckdns", Factory{
		Name: "DuckDNS",
		Description: "Бесплатный поддомен вида имя.duckdns.org. SRV не умеет — " +
			"сервер должен стоять на стандартном порту, иначе игрокам придётся " +
			"вводить порт руками.",
		New: func(cfg Config) (Provider, error) { return NewDuckDNS(cfg, nil) },
	})
	r.Register("cloudflare", Factory{
		Name:        "Cloudflare",
		Description: "Свой домен, уже делегированный на Cloudflare. Умеет всё.",
		NeedsZone:   true,
		New:         func(cfg Config) (Provider, error) { return NewCloudflare(cfg, nil) },
	})

	return r
}

// Register adds a factory. Registering an id twice panics: it is a
// programming error, and the alternative is one provider silently shadowing
// another depending on init order.
func (r *Registry) Register(id string, f Factory) {
	if _, exists := r.factories[id]; exists {
		panic("dns: provider " + id + " is already registered")
	}
	r.factories[id] = f
	r.order = append(r.order, id)
}

// Build constructs the named provider.
func (r *Registry) Build(id string, cfg Config) (Provider, error) {
	factory, ok := r.factories[id]
	if !ok {
		return nil, fmt.Errorf("dns: unknown provider %q (have %s)", id, strings.Join(r.order, ", "))
	}
	return factory.New(cfg)
}

// List returns the registered factories in registration order.
func (r *Registry) List() []struct {
	ID string
	Factory
} {
	out := make([]struct {
		ID string
		Factory
	}, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, struct {
			ID string
			Factory
		}{ID: id, Factory: r.factories[id]})
	}
	return out
}

// effectiveTTL applies the provider's floor to a requested TTL.
func effectiveTTL(requested, floor int) int {
	if requested <= 0 {
		requested = DefaultTTL
	}
	if floor > 0 && requested < floor {
		return floor
	}
	return requested
}
