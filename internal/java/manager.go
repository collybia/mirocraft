package java

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Timeouts. A runtime is 40–60 MB, so the download budget is generous while
// the API call is not.
const (
	apiTimeout      = 30 * time.Second
	downloadTimeout = 15 * time.Minute
	verifyTimeout   = 30 * time.Second
)

// userAgent identifies the panel to Adoptium.
const userAgent = "Mirocraft/1.0 (+https://github.com/collybia/mirocraft)"

// Manager installs runtimes into a directory and keeps track of what is there.
type Manager struct {
	// Dir is where runtimes are unpacked, one subdirectory per major.
	Dir string
	// APIBase is the Adoptium API root, overridable for tests.
	APIBase string
	// Client is the HTTP client used for both the API and the download.
	Client *http.Client
	// Platform overrides the detected host platform, for tests.
	Platform *Platform

	log *slog.Logger

	// installs serializes work per major, so two servers starting at once do
	// not download the same runtime twice or unpack over each other.
	installs sync.Map // major int -> *sync.Mutex

	mu    sync.RWMutex
	known map[int]*Runtime
}

// NewManager returns a manager installing into dir.
func NewManager(dir string, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		Dir:     dir,
		APIBase: AdoptiumAPI,
		Client:  &http.Client{Timeout: downloadTimeout},
		log:     log,
		known:   make(map[int]*Runtime),
	}
}

// adoptiumAsset is one entry of the assets/latest response.
type adoptiumAsset struct {
	ReleaseName string `json:"release_name"`
	Binary      struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		ImageType    string `json:"image_type"`
		Package      struct {
			Name     string `json:"name"`
			Link     string `json:"link"`
			Size     int64  `json:"size"`
			Checksum string `json:"checksum"`
		} `json:"package"`
	} `json:"binary"`
}

// Ensure returns a working runtime for the given major, installing it if it
// is not already present.
//
// Calls for the same major are serialized: two servers starting together
// would otherwise download the same 50 MB twice and unpack into the same
// directory at the same time.
func (m *Manager) Ensure(ctx context.Context, major int) (*Runtime, error) {
	if rt, ok := m.lookup(major); ok {
		return rt, nil
	}

	lock, _ := m.installs.LoadOrStore(major, &sync.Mutex{})
	mutex := lock.(*sync.Mutex)
	mutex.Lock()
	defer mutex.Unlock()

	// Another goroutine may have installed it while this one waited.
	if rt, ok := m.lookup(major); ok {
		return rt, nil
	}

	// An earlier run may have installed it, so disk is checked before the
	// network.
	if rt, err := m.discover(ctx, major); err == nil {
		m.remember(rt)
		return rt, nil
	}

	rt, err := m.install(ctx, major)
	if err != nil {
		return nil, err
	}
	m.remember(rt)
	return rt, nil
}

// Installed returns the runtimes currently known to the manager.
func (m *Manager) Installed() []*Runtime {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]*Runtime, 0, len(m.known))
	for _, rt := range m.known {
		out = append(out, rt)
	}
	return out
}

// Scan discovers runtimes already on disk, so a restart does not re-download
// what a previous run installed.
func (m *Manager) Scan(ctx context.Context) {
	entries, err := os.ReadDir(m.Dir)
	if err != nil {
		return // nothing installed yet
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		major, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		rt, err := m.discover(ctx, major)
		if err != nil {
			continue
		}
		m.remember(rt)
		m.log.Info("found an installed java runtime",
			slog.Int("major", rt.Major), slog.String("release", rt.Release))
	}
}

// discover looks for an already-installed runtime for a major and proves it
// runs before returning it.
func (m *Manager) discover(ctx context.Context, major int) (*Runtime, error) {
	root := m.majorDir(major)

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, ErrNotInstalled
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())

		bin, err := findJavaBinary(dir)
		if err != nil {
			continue
		}

		rt := &Runtime{Major: major, Release: entry.Name(), Dir: dir, Bin: bin}
		if err := rt.Verify(ctx); err != nil {
			// A tree that does not execute is worse than no tree: leaving it
			// would make every start fail with a confusing error.
			m.log.Warn("removing an unusable java runtime",
				slog.String("dir", dir), slog.String("error", err.Error()))
			_ = os.RemoveAll(dir)
			continue
		}
		return rt, nil
	}

	return nil, ErrNotInstalled
}

// install downloads and unpacks a runtime.
func (m *Manager) install(ctx context.Context, major int) (*Runtime, error) {
	platform, err := m.platform()
	if err != nil {
		return nil, err
	}

	asset, err := m.latestAsset(ctx, major, platform)
	if err != nil {
		return nil, err
	}

	m.log.Info("installing a java runtime",
		slog.Int("major", major),
		slog.String("release", asset.ReleaseName),
		slog.String("os", platform.OS), slog.String("arch", platform.Arch),
		slog.Int64("size_bytes", asset.Binary.Package.Size))

	release := normalizeRelease(asset.ReleaseName)
	if release == "" {
		release = strconv.Itoa(major)
	}

	// The per-major directory has to exist before anything is staged inside
	// it, and on a cold start nothing has created it yet.
	if err := os.MkdirAll(m.majorDir(major), 0o750); err != nil {
		return nil, fmt.Errorf("creating the runtime directory for Java %d: %w", major, err)
	}
	dir := filepath.Join(m.majorDir(major), release)

	archive, err := m.download(ctx, asset)
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(archive) }()

	// Unpacked beside the target and moved into place, so an interrupted
	// extraction never leaves a half-runtime that discover would accept.
	staging, err := os.MkdirTemp(m.majorDir(major), ".unpacking-*")
	if err != nil {
		return nil, fmt.Errorf("creating a staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	if err := extract(archive, staging); err != nil {
		return nil, fmt.Errorf("unpacking the java runtime: %w", err)
	}

	if err := os.RemoveAll(dir); err != nil {
		return nil, fmt.Errorf("clearing the target directory: %w", err)
	}
	if err := os.Rename(staging, dir); err != nil {
		return nil, fmt.Errorf("moving the runtime into place: %w", err)
	}

	bin, err := findJavaBinary(dir)
	if err != nil {
		return nil, err
	}

	rt := &Runtime{Major: major, Release: asset.ReleaseName, Dir: dir, Bin: bin}
	if err := rt.Verify(ctx); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("the installed runtime does not run: %w", err)
	}

	m.log.Info("java runtime installed",
		slog.Int("major", major), slog.String("bin", bin))
	return rt, nil
}

// latestAsset asks Adoptium for the newest build of a major on a platform.
func (m *Manager) latestAsset(ctx context.Context, major int, platform Platform) (*adoptiumAsset, error) {
	url := fmt.Sprintf(
		"%s/assets/latest/%d/hotspot?architecture=%s&image_type=%s&os=%s&vendor=eclipse",
		m.base(), major, platform.Arch, ImageJRE, platform.OS)

	apiCtx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(apiCtx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building the Adoptium request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := m.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("asking Adoptium for Java %d: %w", major, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%w: java %d for %s/%s",
			ErrUnsupported, major, platform.OS, platform.Arch)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("asking Adoptium for Java %d: unexpected status %s", major, resp.Status)
	}

	var assets []adoptiumAsset
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&assets); err != nil {
		return nil, fmt.Errorf("decoding the Adoptium response: %w", err)
	}
	if len(assets) == 0 {
		return nil, fmt.Errorf("%w: java %d for %s/%s",
			ErrUnsupported, major, platform.OS, platform.Arch)
	}

	asset := assets[0]
	if asset.Binary.Package.Link == "" {
		return nil, fmt.Errorf("Adoptium returned no download for java %d", major)
	}
	return &asset, nil
}

// download fetches the archive to a temporary file, verifying the checksum
// Adoptium publishes.
func (m *Manager) download(ctx context.Context, asset *adoptiumAsset) (string, error) {
	pkg := asset.Binary.Package

	if err := os.MkdirAll(m.Dir, 0o750); err != nil {
		return "", fmt.Errorf("creating the runtime directory: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pkg.Link, nil)
	if err != nil {
		return "", fmt.Errorf("building the download request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := m.client().Do(req)
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", pkg.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading %s: unexpected status %s", pkg.Name, resp.Status)
	}

	tmp, err := os.CreateTemp(m.Dir, ".download-*"+archiveSuffix(pkg.Name))
	if err != nil {
		return "", fmt.Errorf("creating a temporary file: %w", err)
	}
	path := tmp.Name()

	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, hasher), resp.Body)
	closeErr := tmp.Close()

	if err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("writing the download: %w", err)
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("closing the download: %w", closeErr)
	}

	if pkg.Size > 0 && written != pkg.Size {
		_ = os.Remove(path)
		return "", fmt.Errorf("downloaded %d bytes, Adoptium declared %d", written, pkg.Size)
	}

	if pkg.Checksum != "" {
		sum := hex.EncodeToString(hasher.Sum(nil))
		if !strings.EqualFold(sum, pkg.Checksum) {
			_ = os.Remove(path)
			return "", fmt.Errorf("checksum mismatch for %s: got %s, expected %s",
				pkg.Name, sum, pkg.Checksum)
		}
	} else {
		m.log.Warn("Adoptium published no checksum for this build, integrity unverified",
			slog.String("package", pkg.Name))
	}

	return path, nil
}

// runVersion executes `java -version` to prove the runtime works.
func runVersion(ctx context.Context, bin string) error {
	ctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "-version")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s -version failed: %v (%s)",
			ErrNotExecutable, bin, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m *Manager) majorDir(major int) string {
	return filepath.Join(m.Dir, strconv.Itoa(major))
}

func (m *Manager) base() string {
	if m.APIBase == "" {
		return AdoptiumAPI
	}
	return m.APIBase
}

func (m *Manager) client() *http.Client {
	if m.Client == nil {
		return &http.Client{Timeout: downloadTimeout}
	}
	return m.Client
}

func (m *Manager) platform() (Platform, error) {
	if m.Platform != nil {
		return *m.Platform, nil
	}
	return CurrentPlatform()
}

func (m *Manager) lookup(major int) (*Runtime, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rt, ok := m.known[major]
	return rt, ok
}

func (m *Manager) remember(rt *Runtime) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.known[rt.Major] = rt
}

// archiveSuffix returns the extension that decides how an archive is opened.
func archiveSuffix(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".tar.gz"):
		return ".tar.gz"
	case strings.HasSuffix(lower, ".zip"):
		return ".zip"
	default:
		return filepath.Ext(name)
	}
}

// ErrUnknownArchive is returned for an archive format the manager cannot open.
var ErrUnknownArchive = errors.New("unsupported archive format")
