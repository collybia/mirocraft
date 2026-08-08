package runner

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// os/exec resolves a relative Path against the calling process's directory,
// not against Cmd.Dir. The daemon's data directory is relative by default, so
// the java binary it hands the runner is relative too — and launching it with
// Dir set to the server's folder failed with "the system cannot find the path
// specified", naming a path that plainly existed.
func TestRelativeExecutableIsResolvedAgainstTheDaemonNotTheServerDir(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}

	// The launcher is named relative to the working directory, exactly as the
	// provisioner names the JRE it installed under a relative data_dir.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("reading the working directory: %v", err)
	}
	relative, err := filepath.Rel(cwd, self)
	if err != nil {
		t.Skipf("the test binary is not reachable relatively from %s: %v", cwd, err)
	}
	if filepath.IsAbs(relative) {
		t.Skip("the test binary is on another volume")
	}

	r := NewProcessRunner(slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.Build = func(*Server) (string, []string, error) { return relative, nil, nil }
	r.Env = append(os.Environ(), fakeServerEnv+"=echo")

	// A working directory that is emphatically not the daemon's, which is what
	// makes the relative path resolve differently.
	srv := &Server{ID: "01RELPATH", Name: "relative", Dir: t.TempDir()}

	if err := r.Start(context.Background(), srv); err != nil {
		t.Fatalf("starting with a relative launcher: %v", err)
	}
	t.Cleanup(func() { _ = r.Kill(context.Background(), srv.ID) })

	waitForHistoryLine(t, r, srv.ID, "fake server starting")
}

func TestResolveLaunchPaths(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("reading the working directory: %v", err)
	}

	t.Run("a bare name is left for PATH", func(t *testing.T) {
		// "java" must stay "java": rewriting it to ./java would break every
		// host that relies on the system runtime.
		name, _, err := resolveLaunchPaths("java", "")
		if err != nil {
			t.Fatalf("resolving: %v", err)
		}
		if name != "java" {
			t.Errorf("name = %q, want it untouched", name)
		}
	})

	t.Run("a relative path becomes absolute", func(t *testing.T) {
		name, _, err := resolveLaunchPaths(filepath.Join("data", "java", "bin", "java"), "")
		if err != nil {
			t.Fatalf("resolving: %v", err)
		}
		if !filepath.IsAbs(name) {
			t.Errorf("name = %q, want an absolute path", name)
		}
		if !strings.HasPrefix(name, cwd) {
			t.Errorf("name = %q, want it under %q", name, cwd)
		}
	})

	t.Run("an absolute path is left alone", func(t *testing.T) {
		absolute := filepath.Join(cwd, "java")
		name, _, err := resolveLaunchPaths(absolute, "")
		if err != nil {
			t.Fatalf("resolving: %v", err)
		}
		if name != absolute {
			t.Errorf("name = %q, want %q", name, absolute)
		}
	})

	t.Run("the server directory becomes absolute", func(t *testing.T) {
		_, dir, err := resolveLaunchPaths("java", filepath.Join("data", "servers", "01X"))
		if err != nil {
			t.Fatalf("resolving: %v", err)
		}
		if !filepath.IsAbs(dir) {
			t.Errorf("dir = %q, want an absolute path", dir)
		}
	})

	t.Run("an empty directory stays empty", func(t *testing.T) {
		// Empty means "inherit the daemon's", which is not the same as the
		// daemon's directory spelled out — os/exec treats them alike, but
		// rewriting it would hide a caller that forgot to set one.
		_, dir, err := resolveLaunchPaths("java", "")
		if err != nil {
			t.Fatalf("resolving: %v", err)
		}
		if dir != "" {
			t.Errorf("dir = %q, want it empty", dir)
		}
	})

	if runtime.GOOS == "windows" {
		t.Run("a forward-slash path is recognised on windows", func(t *testing.T) {
			// The provisioner joins with filepath, but configuration and the
			// API both carry forward slashes, so both separators reach here.
			name, _, err := resolveLaunchPaths("data/java/21/bin/java.exe", "")
			if err != nil {
				t.Fatalf("resolving: %v", err)
			}
			if !filepath.IsAbs(name) {
				t.Errorf("name = %q, want an absolute path", name)
			}
		})
	}
}
