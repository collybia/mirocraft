package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/collybia/mirocraft/internal/core"
)

// InstallerJarName is what an installer is called inside a server directory.
//
// Distinct from ServerJarName on purpose: for these cores the download is not
// the server, and a file called server.jar that reinstalls when run is the
// kind of thing someone debugs for an hour.
const InstallerJarName = "installer.jar"

// installTimeout bounds one installer run.
//
// Forge and NeoForge installers download a hundred-odd libraries and patch the
// Minecraft jar, which takes minutes on a slow link. Generous, because the
// alternative is a server that fails to create for a reason that reads like a
// bug in the panel.
const installTimeout = 20 * time.Minute

// installMarkerPrefix names the file that records a finished install.
const installMarkerPrefix = ".mirocraft-installed"

// runInstaller executes a core's installer, once per build.
//
// The marker records which build was installed, so upgrading a server to a
// newer build reinstalls and restarting one does not. Without it every start
// would spend minutes re-downloading libraries that are already there.
func (p *Provisioner) runInstaller(ctx context.Context, provider core.Provider, build *core.Build, dir, javaBin string) error {
	installer, ok := provider.(core.Installer)
	if !ok {
		return fmt.Errorf("%s ships an installer but does not say how to run it", build.Core)
	}

	marker := filepath.Join(dir, installMarkerFor(build))
	if _, err := os.Stat(marker); err == nil {
		return nil
	}

	if javaBin == "" {
		// Only reachable with SkipHostJava, which is what the Docker runner
		// sets: the runtime lives in the image, not on the host. Installing
		// would need a JVM here, so this is refused rather than half-done.
		return fmt.Errorf("%s needs an installer run, which needs a Java runtime on this host", build.Core)
	}

	args := append([]string{"-jar", InstallerJarName}, installer.InstallArgs(build)...)

	installCtx, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()

	p.log.Info("running the core's installer",
		slog.String("core", build.Core), slog.String("version", build.Version),
		slog.String("build", build.Build))

	// #nosec G204 -- the binary is a runtime this daemon installed and the
	// arguments come from the provider, not from a request.
	cmd := exec.CommandContext(installCtx, javaBin, args...)
	cmd.Dir = dir

	output, err := cmd.CombinedOutput()
	if err != nil {
		if errors.Is(installCtx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("the %s installer did not finish within %s", build.Core, installTimeout)
		}
		// The tail rather than everything: these installers print hundreds of
		// lines of library downloads, and the reason is at the end.
		return fmt.Errorf("the %s installer failed: %w\n%s", build.Core, err, tail(string(output), 20))
	}

	if err := os.WriteFile(marker, []byte(build.Build), 0o640); err != nil {
		return fmt.Errorf("recording the %s install: %w", build.Core, err)
	}

	p.log.Info("core installed",
		slog.String("core", build.Core), slog.String("version", build.Version))
	return nil
}

// installMarkerFor names the marker for one build.
func installMarkerFor(build *core.Build) string {
	name := installMarkerPrefix + "-" + build.Core + "-" + build.Version
	if build.Build != "" {
		name += "-" + build.Build
	}
	return name
}

// targetOS is the system the server will run under.
//
// Not always this host's: when Docker is the runner the server runs in a Linux
// container whatever the host is, and Forge's argument files differ between
// the two. The provisioner is told which by the daemon that built it.
func (p *Provisioner) targetOS() string {
	if p.TargetOS != "" {
		return p.TargetOS
	}
	return runtime.GOOS
}

// tail returns the last n lines of s.
func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}
