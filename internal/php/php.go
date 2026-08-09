// Package php installs the PHP runtime PocketMine needs.
//
// Not the distribution's PHP. PocketMine builds its own with the extensions it
// requires and against the version it was compiled for; a stock interpreter
// refuses the phar with a message about a missing extension, which reads as a
// broken download rather than as the wrong PHP. So the panel installs theirs,
// the same way it installs Java runtimes rather than asking for one.
package php

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// BinariesReleases is where PocketMine publishes its PHP builds.
const BinariesReleases = "https://api.github.com/repos/pmmp/PHP-Binaries/releases?per_page=30"

// Timeouts. A PHP build is 30–60 MB.
const (
	apiTimeout      = 30 * time.Second
	downloadTimeout = 10 * time.Minute
)

// userAgent identifies the panel to GitHub.
const userAgent = "Mirocraft/1.0 (+https://github.com/collybia/mirocraft)"

// Errors.
var (
	ErrNotInstalled = errors.New("php runtime is not installed")
	ErrUnsupported  = errors.New("no PocketMine PHP build for this platform")
)

// Runtime is an installed PHP.
type Runtime struct {
	// Version is the PHP feature version, for example "8.2".
	Version string
	// Dir is where it is unpacked.
	Dir string
	// Bin is the absolute path to the php executable.
	Bin string
}

// Manager installs PHP runtimes into a directory.
type Manager struct {
	// Dir is where runtimes are unpacked, one subdirectory per version.
	Dir string
	// ReleasesURL is the GitHub listing, overridable for tests.
	ReleasesURL string
	// Client is used for both the API and the download.
	Client *http.Client
	// GOOS and GOARCH override the detected platform, for tests.
	GOOS   string
	GOARCH string

	log *slog.Logger

	// installs serializes work per version, so two servers starting together
	// do not download the same runtime twice.
	installs sync.Map

	mu    sync.RWMutex
	known map[string]*Runtime
}

// NewManager returns a manager installing into dir.
func NewManager(dir string, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		Dir:         dir,
		ReleasesURL: BinariesReleases,
		Client:      &http.Client{Timeout: downloadTimeout},
		log:         log,
		known:       make(map[string]*Runtime),
	}
}

// Ensure returns a working PHP for the given feature version, installing it if
// it is not already present.
func (m *Manager) Ensure(ctx context.Context, version string) (*Runtime, error) {
	if rt, ok := m.lookup(version); ok {
		return rt, nil
	}

	lock, _ := m.installs.LoadOrStore(version, &sync.Mutex{})
	mutex, ok := lock.(*sync.Mutex)
	if !ok {
		return nil, fmt.Errorf("php: the install lock for %s is not a mutex", version)
	}
	mutex.Lock()
	defer mutex.Unlock()

	if rt, ok := m.lookup(version); ok {
		return rt, nil
	}
	if rt, err := m.discover(version); err == nil {
		m.remember(rt)
		return rt, nil
	}

	rt, err := m.install(ctx, version)
	if err != nil {
		return nil, err
	}
	m.remember(rt)
	return rt, nil
}

// discover looks for an already-installed runtime.
func (m *Manager) discover(version string) (*Runtime, error) {
	dir := filepath.Join(m.Dir, version)
	bin := filepath.Join(dir, "bin", "php7", "bin", "php")
	if m.goos() == "windows" {
		bin = filepath.Join(dir, "bin", "php", "php.exe")
	}

	if _, err := os.Stat(bin); err != nil {
		return nil, ErrNotInstalled
	}
	return &Runtime{Version: version, Dir: dir, Bin: bin}, nil
}

// install downloads and unpacks a runtime.
func (m *Manager) install(ctx context.Context, version string) (*Runtime, error) {
	asset, err := m.findAsset(ctx, version)
	if err != nil {
		return nil, err
	}

	m.log.Info("installing a php runtime",
		slog.String("version", version), slog.String("asset", asset.Name))

	archive, err := m.download(ctx, asset.URL, asset.Name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(archive) }()

	dir := filepath.Join(m.Dir, version)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("creating the php directory: %w", err)
	}
	// Unpacked into a staging directory and moved, so an interrupted install
	// never leaves half a runtime that discover would accept.
	staging, err := os.MkdirTemp(m.Dir, ".unpacking-*")
	if err != nil {
		return nil, fmt.Errorf("creating a staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	if err := extract(archive, staging); err != nil {
		return nil, fmt.Errorf("unpacking the php runtime: %w", err)
	}

	if err := os.RemoveAll(dir); err != nil {
		return nil, fmt.Errorf("clearing the target directory: %w", err)
	}
	if err := os.Rename(staging, dir); err != nil {
		return nil, fmt.Errorf("moving the php runtime into place: %w", err)
	}

	rt, err := m.discover(version)
	if err != nil {
		return nil, fmt.Errorf("the installed php runtime has no usable binary: %w", err)
	}
	m.log.Info("php runtime installed",
		slog.String("version", version), slog.String("bin", rt.Bin))
	return rt, nil
}

// githubAsset is one downloadable file.
type githubAsset struct {
	Name string
	URL  string
}

// findAsset picks the build for this platform and PHP version.
//
// The tags read pm5-php-8.2-latest and the files PHP-8.2-Linux-x86_64-PM5.tar.gz,
// so the version and the platform are both in names rather than in fields.
func (m *Manager) findAsset(ctx context.Context, version string) (*githubAsset, error) {
	var releases []struct {
		TagName    string `json:"tag_name"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}

	apiCtx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(apiCtx, http.MethodGet, m.releases(), nil)
	if err != nil {
		return nil, fmt.Errorf("building the PHP-Binaries request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := m.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("asking for PHP %s: %w", version, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("asking for PHP %s: unexpected status %s", version, resp.Status)
	}
	if err := decodeJSON(resp.Body, &releases); err != nil {
		return nil, fmt.Errorf("reading the PHP-Binaries releases: %w", err)
	}

	want := m.assetInfix(version)
	for _, release := range releases {
		if release.Prerelease {
			continue
		}
		for _, asset := range release.Assets {
			// Debugging symbols are published beside the runtime and are three
			// times its size.
			if strings.HasPrefix(asset.Name, "Z-") {
				continue
			}
			if strings.Contains(asset.Name, want) {
				return &githubAsset{Name: asset.Name, URL: asset.URL}, nil
			}
		}
	}
	return nil, fmt.Errorf("%w: php %s for %s/%s", ErrUnsupported, version, m.goos(), m.goarch())
}

// assetInfix is the part of a file name that identifies this platform and
// version: PHP-8.2-Linux-x86_64.
func (m *Manager) assetInfix(version string) string {
	system := "Linux"
	switch m.goos() {
	case "windows":
		system = "Windows"
	case "darwin":
		system = "MacOS"
	}

	arch := "x86_64"
	switch m.goarch() {
	case "arm64":
		arch = "arm64"
	case "amd64":
		if system == "Windows" {
			arch = "x64"
		}
	}
	return fmt.Sprintf("PHP-%s-%s-%s", version, system, arch)
}

// download fetches an asset to a temporary file.
func (m *Manager) download(ctx context.Context, url, name string) (string, error) {
	downloadCtx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(downloadCtx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("building the php download request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := m.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("downloading php: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading php: unexpected status %s", resp.Status)
	}

	if err := os.MkdirAll(m.Dir, 0o750); err != nil {
		return "", fmt.Errorf("creating the php directory: %w", err)
	}
	file, err := os.CreateTemp(m.Dir, "php-*-"+name)
	if err != nil {
		return "", fmt.Errorf("creating a temporary file: %w", err)
	}
	path := file.Name()

	_, err = io.Copy(file, resp.Body)
	closeErr := file.Close()
	if err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("downloading php: %w", err)
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("downloading php: %w", closeErr)
	}
	return path, nil
}

func (m *Manager) lookup(version string) (*Runtime, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rt, ok := m.known[version]
	return rt, ok
}

func (m *Manager) remember(rt *Runtime) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.known[rt.Version] = rt
}

func (m *Manager) client() *http.Client {
	if m.Client == nil {
		return &http.Client{Timeout: downloadTimeout}
	}
	return m.Client
}

func (m *Manager) releases() string {
	if m.ReleasesURL == "" {
		return BinariesReleases
	}
	return m.ReleasesURL
}

func (m *Manager) goos() string {
	if m.GOOS != "" {
		return m.GOOS
	}
	return runtime.GOOS
}

func (m *Manager) goarch() string {
	if m.GOARCH != "" {
		return m.GOARCH
	}
	return runtime.GOARCH
}
