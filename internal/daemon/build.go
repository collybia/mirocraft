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
	"time"

	"github.com/collybia/mirocraft/internal/core"
)

// buildWorkDir is the scratch directory compiles happen in, under the cache.
//
// Shared and per-version rather than per-server: BuildTools clones several
// repositories and leaves a gigabyte behind, and doing that again for the
// second Spigot server on the same version would be a gigabyte and twenty
// minutes spent to produce a file that already exists.
const buildWorkDir = "build"

// ErrBuildToolMissing reports that compiling needs something this machine does
// not have.
var ErrBuildToolMissing = errors.New("a program needed to build this core is missing")

// buildLocally compiles a core and returns the path of the jar it produced.
//
// The result is placed in the artifact cache under the same key a download
// would use, so everything downstream — the copy into the server directory,
// the reuse by a second server — works the same either way.
func (p *Provisioner) buildLocally(ctx context.Context, provider core.Provider, build *core.Build, javaBin string) (string, error) {
	builder, ok := provider.(core.LocalBuilder)
	if !ok {
		return "", fmt.Errorf("%s has to be compiled but does not say how", build.Core)
	}

	cached := p.Downloader.CachePath(build)
	if info, err := os.Stat(cached); err == nil && info.Size() > 0 {
		p.log.Debug("using the previously built core",
			slog.String("core", build.Core), slog.String("version", build.Version))
		return cached, nil
	}

	if javaBin == "" {
		return "", fmt.Errorf("%s has to be compiled, which needs a Java runtime on this host", build.Core)
	}
	// BuildTools clones the sources, so git has to be there — except on
	// Windows, where it downloads a portable git of its own. Observed rather
	// than assumed: the first run here pulled PortableGit into the work
	// directory, and refusing on this host would have refused a build that
	// works. Checked at all because the tool's own failure for a missing git
	// is a stack trace several hundred lines into a twenty-minute run.
	if runtime.GOOS != "windows" {
		if _, err := exec.LookPath("git"); err != nil {
			return "", fmt.Errorf("%w: %s is compiled by a tool that needs git installed on this machine",
				ErrBuildToolMissing, build.Core)
		}
	}

	tool, err := builder.Tool(ctx)
	if err != nil {
		return "", fmt.Errorf("finding the tool that builds %s: %w", build.Core, err)
	}
	toolPath, err := p.Downloader.Fetch(ctx, tool)
	if err != nil {
		return "", fmt.Errorf("downloading the tool that builds %s: %w", build.Core, err)
	}

	work := filepath.Join(p.Downloader.Dir, buildWorkDir, sanitize(build.Core), sanitize(build.Version))
	if err := os.MkdirAll(work, 0o750); err != nil {
		return "", fmt.Errorf("creating the build directory: %w", err)
	}

	// Copied in rather than run from the cache: the tool writes its working
	// files beside itself, and a cache directory is not a scratch directory.
	localTool := filepath.Join(work, filepath.Base(toolPath))
	if err := copyFile(toolPath, localTool); err != nil {
		return "", fmt.Errorf("staging the build tool: %w", err)
	}

	buildCtx, cancel := context.WithTimeout(ctx, core.BuildTimeout)
	defer cancel()

	args := append([]string{"-jar", filepath.Base(localTool)}, builder.BuildArgs(build)...)
	p.log.Info("compiling a core; this takes minutes",
		slog.String("core", build.Core), slog.String("version", build.Version),
		slog.String("dir", work))

	started := time.Now()
	// #nosec G204 -- the binary is a runtime this daemon installed and the
	// arguments come from the provider, not from a request.
	cmd := exec.CommandContext(buildCtx, javaBin, args...)
	cmd.Dir = work

	output, err := cmd.CombinedOutput()
	if err != nil {
		if errors.Is(buildCtx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("compiling %s %s did not finish within %s",
				build.Core, build.Version, core.BuildTimeout)
		}
		return "", fmt.Errorf("compiling %s %s failed: %w\n%s",
			build.Core, build.Version, err, tail(string(output), 20))
	}

	produced := filepath.Join(work, builder.Produced(build))
	if _, err := os.Stat(produced); err != nil {
		return "", fmt.Errorf("the build of %s %s left no %s: %w",
			build.Core, build.Version, builder.Produced(build), err)
	}

	if err := os.MkdirAll(filepath.Dir(cached), 0o750); err != nil {
		return "", fmt.Errorf("creating the cache directory: %w", err)
	}
	if err := copyFile(produced, cached); err != nil {
		return "", fmt.Errorf("caching the built core: %w", err)
	}

	p.log.Info("core compiled",
		slog.String("core", build.Core), slog.String("version", build.Version),
		slog.Duration("took", time.Since(started).Round(time.Second)))
	return cached, nil
}

// sanitize keeps a path component to something safe to join.
func sanitize(part string) string {
	cleaned := filepath.Base(filepath.Clean(part))
	if cleaned == "." || cleaned == string(filepath.Separator) {
		return "unknown"
	}
	return cleaned
}
