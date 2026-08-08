// Package core knows how to obtain server software: which versions exist for
// a core, which build to use, where to download it and what runtime it needs.
//
// Every core the panel supports is a Provider. The daemon only talks to the
// Registry, so adding Purpur or Fabric later means adding a file here and
// registering it — nothing outside this package changes.
package core

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Kind decides a server's default port, protocol and console behaviour.
type Kind string

const (
	KindServer  Kind = "server"
	KindProxy   Kind = "proxy"
	KindBedrock Kind = "bedrock"
)

// Runtime is what has to be installed for the software to run.
type Runtime string

const (
	RuntimeJava   Runtime = "java"
	RuntimePHP    Runtime = "php"
	RuntimeNative Runtime = "native"
)

// Release channels.
const (
	ChannelRelease  = "release"
	ChannelSnapshot = "snapshot"
)

// Provider errors.
var (
	ErrUnknownProvider = errors.New("unknown core")
	ErrUnknownVersion  = errors.New("version is not available for this core")
	ErrNoBuilds        = errors.New("no builds published for this version")
	ErrChecksum        = errors.New("downloaded file failed its checksum")
)

// Version is one Minecraft version a provider can serve.
type Version struct {
	// ID is the Minecraft version, for example "1.21.4".
	ID string `json:"id"`
	// Channel is release or snapshot.
	Channel string `json:"channel"`
	// ReleasedAt is when it was published, where upstream says.
	ReleasedAt time.Time `json:"released_at,omitzero"`
	// JavaMajor is the Java version needed to run it.
	JavaMajor int `json:"java_major"`
}

// Build is a concrete downloadable artifact.
type Build struct {
	// Core is the provider id, for example "paper".
	Core string `json:"core"`
	// Version is the Minecraft version.
	Version string `json:"version"`
	// Build identifies the build within a version. Empty for cores that
	// publish one artifact per version, as vanilla does.
	Build string `json:"build,omitempty"`

	URL      string `json:"url"`
	FileName string `json:"file_name"`

	// Checksum as published upstream, with the algorithm that produced it.
	// Empty means upstream publishes none — recorded honestly rather than
	// hidden, because it changes what a download can promise.
	Checksum  string `json:"checksum,omitempty"`
	Algorithm string `json:"algorithm,omitempty"`

	SizeBytes int64 `json:"size_bytes,omitempty"`
	JavaMajor int   `json:"java_major"`
}

// Verifiable reports whether upstream published a checksum for this build.
func (b *Build) Verifiable() bool {
	return b.Checksum != "" && b.Algorithm != ""
}

// Content describes the add-ons a core accepts.
//
// The core knows this, so it says it: the alternative is a table elsewhere
// mapping core ids to loaders, which then has to be edited every time a core
// is added — and forgetting to would silently offer the wrong plugins.
type Content struct {
	// Loader is the loader id used by the add-on registries — paper, fabric,
	// forge, velocity and so on. Empty means the core takes no add-ons at all,
	// which is the honest answer for vanilla: a plugin jar dropped next to it
	// is simply never read.
	Loader string
	// Dir is where artifacts go, relative to the server directory. Bukkit-
	// family servers read plugins/, mod loaders read mods/.
	Dir string
}

// Accepts reports whether add-ons can be installed at all.
func (c Content) Accepts() bool { return c.Loader != "" && c.Dir != "" }

// Provider serves one core.
type Provider interface {
	// ID is the stable identifier used in the API and the database.
	ID() string
	// Name is what the panel shows.
	Name() string
	Kind() Kind
	Runtime() Runtime

	// Content says what add-ons this core takes and where they live.
	Content() Content

	// Versions lists what can be installed, newest first.
	Versions(ctx context.Context) ([]Version, error)

	// Resolve picks the build to install for a Minecraft version. An empty
	// version means the latest release.
	Resolve(ctx context.Context, version string) (*Build, error)
}

// Registry holds the providers the daemon knows about.
type Registry struct {
	mu        sync.RWMutex
	providers map[string]Provider
	order     []string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{providers: make(map[string]Provider)}
}

// Register adds a provider. Registering an id twice panics: it is a
// programming error, and the alternative is one core silently shadowing
// another depending on init order.
func (r *Registry) Register(p Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.providers[p.ID()]; exists {
		panic(fmt.Sprintf("core: provider %q is already registered", p.ID()))
	}
	r.providers[p.ID()] = p
	r.order = append(r.order, p.ID())
}

// Get returns a provider by id.
func (r *Registry) Get(id string) (Provider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	p, ok := r.providers[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownProvider, id)
	}
	return p, nil
}

// List returns every provider in registration order, which is the order the
// panel shows them in.
func (r *Registry) List() []Provider {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Provider, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.providers[id])
	}
	return out
}

// IDs returns the registered provider ids, sorted.
func (r *Registry) IDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := append([]string(nil), r.order...)
	sort.Strings(out)
	return out
}
