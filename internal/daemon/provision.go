package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/collybia/mirocraft/internal/core"
	"github.com/collybia/mirocraft/internal/gamefiles"
	"github.com/collybia/mirocraft/internal/java"
	"github.com/collybia/mirocraft/internal/store"
)

// ServerJarName is what the downloaded core is called inside a server
// directory, whatever upstream named the artifact.
//
// A fixed name keeps the launch command stable across updates: an operator
// who wrote a script around it does not have to chase paper-26.2-111.jar
// becoming paper-26.2-112.jar.
const ServerJarName = "server.jar"

// Provisioner puts everything a server needs in place before it starts: the
// core jar in its directory and a Java runtime that can run it.
//
// This is where the three halves meet — the core registry knows what to
// download, the Java manager knows what runs it, and the runner only knows
// how to start a process — so none of them has to know about the others.
type Provisioner struct {
	Cores      *core.Registry
	Downloader *core.Downloader
	Java       *java.Manager

	// SkipHostJava stops the host runtime being downloaded. Set when servers
	// run in containers: the image brings its own Java, so fetching 110 MB of
	// JRE onto the host for a server that will never touch it is pure waste.
	//
	// Cores that ship an installer are the exception, and a deliberate one:
	// the installer writes into the server directory before any container
	// exists, so it has to run on the host. Such a server pays for a host
	// runtime once; every other server under Docker still pays nothing.
	SkipHostJava bool

	// TargetOS is the system servers will run under, "linux" or "windows".
	// Empty means this host's. Set to linux when Docker is the runner: the
	// server runs in a container whatever the host is, and Forge's argument
	// files differ between the two.
	TargetOS string

	log *slog.Logger
}

// NewProvisioner returns a provisioner.
func NewProvisioner(cores *core.Registry, downloader *core.Downloader, javaMgr *java.Manager, log *slog.Logger) *Provisioner {
	if log == nil {
		log = slog.Default()
	}
	return &Provisioner{Cores: cores, Downloader: downloader, Java: javaMgr, log: log}
}

// Launch is what a server needs in order to start.
type Launch struct {
	// JarName is the jar to run, relative to the server directory. Empty when
	// Args says how to start instead.
	JarName string
	// Args replaces the usual "-jar server.jar nogui" for cores that start
	// another way — Forge and NeoForge start through an argument file the
	// installer wrote. Empty means the usual invocation.
	Args []string
	// JavaBin is the absolute path to the java executable.
	JavaBin string
	// JavaMajor is the runtime's feature version, for logs and the API.
	JavaMajor int
	// Build records what was installed.
	Build *core.Build
}

// Prepare makes a server ready to start, downloading whatever is missing.
//
// It is safe to call before every start: the core jar and the Java runtime
// are both cached, so a server that is already provisioned only pays for a
// checksum check.
// dir is passed in rather than read from the record: where a server's files
// live is decided by the current data directory, not by whatever absolute
// path happened to be stored when the server was created.
func (p *Provisioner) Prepare(ctx context.Context, srv *store.Server, dir string) (*Launch, error) {
	if dir == "" {
		return nil, errors.New("server has no directory")
	}

	provider, err := p.Cores.Get(srv.Core)
	if err != nil {
		return nil, err
	}

	build, err := provider.Resolve(ctx, srv.Version)
	if err != nil {
		return nil, fmt.Errorf("resolving %s %s: %w", srv.Core, srv.Version, err)
	}

	jarPath := filepath.Join(dir, ServerJarName)
	if build.NeedsInstall() {
		jarPath = filepath.Join(dir, InstallerJarName)
	}
	if err := p.ensureJar(ctx, build, jarPath); err != nil {
		return nil, err
	}

	launch := &Launch{JarName: ServerJarName, JavaMajor: build.JavaMajor, Build: build}

	// A host runtime is needed to run the server, or — even under Docker — to
	// run an installer, which writes into the directory before any container
	// exists.
	installJava := ""
	if !p.SkipHostJava || build.NeedsInstall() {
		runtime, err := p.Java.Ensure(ctx, build.JavaMajor)
		if err != nil {
			return nil, fmt.Errorf("preparing Java %d for %s %s: %w",
				build.JavaMajor, srv.Core, srv.Version, err)
		}
		installJava = runtime.Bin
		if !p.SkipHostJava {
			launch.JavaBin = runtime.Bin
			// The installed runtime wins over the table: it is what will
			// actually run, and the two can differ where the panel installs a
			// newer version than the minimum.
			launch.JavaMajor = runtime.Major
		}
	}

	// After the runtime is in place, because running an installer needs one.
	if build.NeedsInstall() {
		if err := p.runInstaller(ctx, provider, build, dir, installJava); err != nil {
			return nil, err
		}
	}

	if launcher, ok := provider.(core.Launcher); ok {
		args, err := launcher.LaunchArgs(dir, build, p.targetOS())
		if err != nil {
			return nil, fmt.Errorf("working out how to start %s %s: %w", srv.Core, srv.Version, err)
		}
		launch.Args = args
		// The jar name is cleared so nothing downstream falls back to running
		// a jar that is not the server: for these cores server.jar is the
		// installer, and running it again would reinstall rather than start.
		launch.JarName = ""
	}

	// The panel refuses to let anyone edit server-port through the settings
	// API on the grounds that it owns it, so it has to own it: without this
	// the record says one port and the server listens on another, two servers
	// created on different ports both bind 25565, and the second fails to
	// start with an address already in use.
	if err := p.applyManagedProperties(srv, dir); err != nil {
		return nil, err
	}

	p.log.Info("server prepared",
		slog.String("server_id", srv.ID),
		slog.String("core", build.Core), slog.String("version", build.Version),
		slog.String("build", build.Build), slog.Int("java", launch.JavaMajor))

	return launch, nil
}

// applyManagedProperties writes the server.properties keys the panel owns.
//
// Run before every start rather than once at creation: an operator who edits
// the file by hand, or restores a backup made on another port, would otherwise
// leave the server listening somewhere the panel does not know about.
func (p *Provisioner) applyManagedProperties(srv *store.Server, dir string) error {
	if srv.Port <= 0 {
		return nil
	}

	properties, err := gamefiles.LoadProperties(dir)
	if err != nil {
		return fmt.Errorf("reading server.properties of %s: %w", srv.ID, err)
	}

	port := strconv.Itoa(srv.Port)
	if current, ok := properties.Get("server-port"); ok && current == port {
		return nil
	}

	properties.Set("server-port", port)
	// Left empty on purpose: an empty server-ip binds every interface, which
	// is what a panel-managed server needs. A value here would silently make
	// the server unreachable from anywhere but that address.
	properties.Set("server-ip", "")

	if err := properties.Save(dir); err != nil {
		return fmt.Errorf("writing server.properties of %s: %w", srv.ID, err)
	}
	p.log.Info("applied the panel's port to server.properties",
		slog.String("server_id", srv.ID), slog.Int("port", srv.Port))
	return nil
}

// ensureJar makes sure the server directory holds the right jar.
//
// The jar is copied out of the shared cache rather than linked: a server
// directory is the operator's to mess with, and a hard link would mean
// editing or replacing one server's jar silently changed every other server
// on the same version.
func (p *Provisioner) ensureJar(ctx context.Context, build *core.Build, jarPath string) error {
	cached, err := p.Downloader.Fetch(ctx, build)
	if err != nil {
		return fmt.Errorf("downloading %s %s: %w", build.Core, build.Version, err)
	}

	source, err := os.Stat(cached)
	if err != nil {
		return fmt.Errorf("reading the cached jar: %w", err)
	}

	// Same size means the same jar in practice, and the cache has already
	// verified its checksum, so re-copying 60 MB on every start is waste.
	if existing, err := os.Stat(jarPath); err == nil && existing.Size() == source.Size() {
		return nil
	}

	if err := copyFile(cached, jarPath); err != nil {
		return fmt.Errorf("installing the jar into the server directory: %w", err)
	}
	return nil
}

// copyFile writes src to dst through a temporary file in the same directory,
// so an interrupted copy cannot leave a truncated jar that the next start
// would try to run.
func copyFile(src, dst string) error {
	// Both paths are built by the provisioner from a server's own directory.
	in, err := os.Open(src) // #nosec G304 -- a path the provisioner constructed
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(dst), ".jar-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath) // a no-op once renamed
	}()

	if _, err := io.Copy(tmp, in); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o640); err != nil {
		return err
	}
	return os.Rename(tmpPath, dst)
}

// PrepareTimeout bounds a provisioning run. A cold start downloads a core and
// a Java runtime, roughly 110 MB together.
const PrepareTimeout = 20 * time.Minute
