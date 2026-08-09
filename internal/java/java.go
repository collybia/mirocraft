// Package java installs and tracks Java runtimes.
//
// A Minecraft server needs a specific Java major, and the one on the host is
// usually the wrong one or absent entirely: 1.16.5 needs 8, 1.20.4 needs 17,
// 1.21 needs 21 and 26.x needs 25. Asking an operator to install four JVMs
// and keep them straight defeats the point of a one-command install, so the
// panel fetches them itself.
//
// Runtimes come from Adoptium (Eclipse Temurin), and the JRE image is used
// rather than the JDK: a server only runs Java, and the JRE is roughly a
// quarter of the size.
package java

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// AdoptiumAPI is the Adoptium v3 API root.
const AdoptiumAPI = "https://api.adoptium.net/v3"

// The Adoptium image types.
//
// A server runs on the JRE, which is what the panel installs: the JDK is four
// times the download for tools a server never invokes.
//
// The exception is a core that has to be compiled. BuildTools runs javac, and
// a JRE has no compiler — maven fails with "No compiler is provided in this
// environment", several minutes into a build. Found by running it, which is
// the only way it could have been found.
const (
	ImageJRE = "jre"
	ImageJDK = "jdk"
)

// Errors.
var (
	ErrNotInstalled  = errors.New("java runtime is not installed")
	ErrUnsupported   = errors.New("no Temurin build for this platform")
	ErrNotExecutable = errors.New("the installed runtime has no usable java binary")
)

// Runtime is an installed Java runtime.
type Runtime struct {
	// Major is the feature version: 8, 17, 21, 25.
	Major int `json:"major"`
	// Release is the full Temurin release name, for example jdk-21.0.12+8.
	Release string `json:"release"`
	// Dir is the unpacked runtime root.
	Dir string `json:"dir"`
	// Bin is the absolute path to the java executable.
	Bin string `json:"bin"`
}

// Platform identifies a build target in Adoptium's vocabulary.
type Platform struct {
	OS   string // linux, windows, mac
	Arch string // x64, aarch64, arm
}

// CurrentPlatform maps the running host onto Adoptium's names.
//
// Go and Adoptium disagree on spelling — amd64 against x64, arm64 against
// aarch64 — and getting it wrong yields a 404 that reads like the version
// does not exist.
func CurrentPlatform() (Platform, error) {
	var p Platform

	switch runtime.GOOS {
	case "linux":
		p.OS = "linux"
	case "windows":
		p.OS = "windows"
	case "darwin":
		p.OS = "mac"
	default:
		return p, fmt.Errorf("%w: os %s", ErrUnsupported, runtime.GOOS)
	}

	switch runtime.GOARCH {
	case "amd64":
		p.Arch = "x64"
	case "arm64":
		p.Arch = "aarch64"
	case "arm":
		p.Arch = "arm"
	default:
		return p, fmt.Errorf("%w: architecture %s", ErrUnsupported, runtime.GOARCH)
	}

	return p, nil
}

// binaryName is the java executable's name on this platform.
func binaryName() string {
	if runtime.GOOS == "windows" {
		return "java.exe"
	}
	return "java"
}

// findJavaBinary locates the java executable inside an unpacked runtime.
//
// Temurin archives contain a single top-level directory whose name carries
// the release, and macOS adds a Contents/Home level on top of that, so the
// binary is searched for rather than assumed to be at a fixed depth.
func findJavaBinary(root string) (string, error) {
	name := binaryName()

	var found string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Name() != name {
			return nil
		}
		// Only the one in bin/, so a stray copy elsewhere in the archive is
		// not mistaken for the real thing.
		if filepath.Base(filepath.Dir(path)) != "bin" {
			return nil
		}
		found = path
		return filepath.SkipAll
	})
	if err != nil {
		return "", fmt.Errorf("searching for %s under %s: %w", name, root, err)
	}
	if found == "" {
		return "", fmt.Errorf("%w: no bin/%s under %s", ErrNotExecutable, name, root)
	}
	return found, nil
}

// Verify runs the runtime with -version to prove it actually executes.
//
// An unpacked archive is not the same as a working runtime: the wrong
// architecture, a missing libc or a partially extracted tree all produce a
// tree of files that only fails when a server tries to start, which surfaces
// as an unexplained crash rather than an installation error.
func (r *Runtime) Verify(ctx context.Context) error {
	if r.Bin == "" {
		return ErrNotExecutable
	}
	info, err := os.Stat(r.Bin)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrNotExecutable, r.Bin)
	}
	if info.IsDir() {
		return fmt.Errorf("%w: %s is a directory", ErrNotExecutable, r.Bin)
	}
	return runVersion(ctx, r.Bin)
}

// String describes the runtime for logs.
func (r *Runtime) String() string {
	if r.Release != "" {
		return fmt.Sprintf("Java %d (%s)", r.Major, r.Release)
	}
	return fmt.Sprintf("Java %d", r.Major)
}

// normalizeRelease turns a Temurin release name into something safe to use as
// a directory name.
func normalizeRelease(release string) string {
	var b strings.Builder
	for _, r := range release {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '-' || r == '_':
			b.WriteRune(r)
		default:
			// '+' in jdk-21.0.12+8 is legal on disk but awkward in paths and
			// URLs, so it becomes a separator like everything else unusual.
			b.WriteRune('_')
		}
	}

	out := b.String()
	if strings.Trim(out, ".") == "" {
		return ""
	}
	return out
}
