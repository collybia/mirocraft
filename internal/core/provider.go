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

// The kinds of software the panel installs.
const (
	KindServer  Kind = "server"
	KindProxy   Kind = "proxy"
	KindBedrock Kind = "bedrock"
)

// Runtime is what has to be installed for the software to run.
type Runtime string

// The runtimes a core can need.
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

// Artifact says what the downloaded file actually is.
type Artifact string

const (
	// ArtifactServer is a jar that is the server. Vanilla, Paper and their
	// forks ship these: a few dozen megabytes that run offline.
	ArtifactServer Artifact = "server"
	// ArtifactInstaller is a jar that has to be executed to produce the
	// server. Forge, NeoForge and Quilt ship these: running the download
	// directly does nothing useful, and what it writes into the directory is
	// what actually gets started.
	ArtifactInstaller Artifact = "installer"
	// ArtifactSource means there is nothing to download: the jar is compiled
	// on this machine. Spigot is the only one — SpigotMC cannot redistribute
	// Mojang's code, so they ship a tool that assembles the server locally.
	ArtifactSource Artifact = "source"
	// ArtifactLauncher is a small jar that fetches the rest on first start.
	// Fabric ships one of these — under two hundred kilobytes, and it needs
	// the network the first time it runs.
	//
	// Recorded because it changes what the panel can promise: a server whose
	// first start needs the internet fails differently from one that does
	// not, and a size check written for a server jar rejects a launcher.
	ArtifactLauncher Artifact = "launcher"
)

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

	// Artifact says whether the file is the server or a launcher for it.
	// Empty means ArtifactServer, which is what most cores publish.
	Artifact Artifact `json:"artifact,omitempty"`
}

// IsLauncher reports whether the first start of this build needs the network.
func (b *Build) IsLauncher() bool { return b.Artifact == ArtifactLauncher }

// NeedsInstall reports whether the download has to be executed before the
// server exists.
func (b *Build) NeedsInstall() bool { return b.Artifact == ArtifactInstaller }

// NeedsBuild reports whether the jar has to be compiled on this machine.
func (b *Build) NeedsBuild() bool { return b.Artifact == ArtifactSource }

// LocalBuilder is implemented by a provider whose core is compiled rather than
// downloaded.
//
// The compile happens once per Minecraft version and the result is cached like
// any other artifact: it takes minutes and a gigabyte of scratch space, and
// paying that per server would make the second Spigot server as slow as the
// first.
type LocalBuilder interface {
	// Tool describes the downloadable program that does the compiling.
	Tool(ctx context.Context) (*Build, error)
	// BuildArgs are the arguments passed after -jar <tool>, run in a scratch
	// directory.
	BuildArgs(build *Build) []string
	// Produced names the jar the tool leaves in that directory.
	Produced(build *Build) string
}

// Installer is implemented by a provider whose download is an installer.
//
// Kept off Provider because most cores are not like this, and an interface
// every implementation has to satisfy with an empty method is an interface
// that teaches the reader nothing.
type Installer interface {
	// InstallArgs are the arguments passed after -jar <downloaded installer>,
	// with the server directory as the working directory.
	InstallArgs(build *Build) []string
}

// The operating systems a server can be launched under.
//
// Not the host's: when Docker is the runner the server runs inside a Linux
// container whatever the host is, and the launch arguments differ between the
// two. Passing it explicitly is what keeps that from being guessed wrong.
const (
	TargetLinux   = "linux"
	TargetWindows = "windows"
)

// Launcher is implemented by a provider whose server does not start with
// -jar server.jar.
//
// Modern Forge and NeoForge start through an argument file the installer
// wrote, which lists a classpath of several hundred libraries — far past what
// a command line holds on Windows, which is why they stopped shipping a
// runnable jar in the first place.
type Launcher interface {
	// LaunchArgs returns the arguments that follow the JVM flags, replacing
	// the usual -jar server.jar nogui. dir is the server directory and
	// targetOS is the system the server will run under.
	LaunchArgs(dir string, build *Build, targetOS string) ([]string, error)
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
