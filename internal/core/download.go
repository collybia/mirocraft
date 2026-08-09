package core

import (
	"context"
	"crypto/sha1" //nolint:gosec // upstream publishes sha1 digests; verifying against them is the point
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Checksum algorithms upstream publishes.
const (
	AlgoSHA1   = "sha1"
	AlgoSHA256 = "sha256"
)

// downloadTimeout bounds a single artifact fetch. Server jars run to a few
// hundred megabytes on a slow link, so this is generous.
const downloadTimeout = 15 * time.Minute

// userAgent identifies the panel to the upstream APIs. PaperMC in particular
// asks for a real one and rate-limits anonymous clients harder.
var userAgent = "Mirocraft/1.0 (+https://github.com/collybia/mirocraft)"

// Downloader fetches build artifacts into a content cache.
//
// The cache is keyed by core, version and build, so re-creating a server with
// the same core does not re-download hundreds of megabytes, and a jar that
// was already verified is not verified again.
type Downloader struct {
	// Dir is the cache root.
	Dir string
	// Client is the HTTP client used. Nil means a default with a timeout.
	Client *http.Client

	log *slog.Logger
}

// NewDownloader returns a downloader caching under dir.
func NewDownloader(dir string, log *slog.Logger) *Downloader {
	if log == nil {
		log = slog.Default()
	}
	return &Downloader{
		Dir:    dir,
		Client: &http.Client{Timeout: downloadTimeout},
		log:    log,
	}
}

// CachePath is where a build is stored once downloaded.
func (d *Downloader) CachePath(b *Build) string {
	version := sanitizePathPart(b.Version)
	build := sanitizePathPart(b.Build)
	if build == "" {
		build = "default"
	}
	name := sanitizePathPart(b.FileName)
	if name == "" {
		name = "server.jar"
	}
	return filepath.Join(d.Dir, sanitizePathPart(b.Core), version, build, name)
}

// Fetch returns the local path of the artifact, downloading it if the cache
// does not already hold a verified copy.
func (d *Downloader) Fetch(ctx context.Context, b *Build) (string, error) {
	path := d.CachePath(b)

	if ok, err := d.cached(path, b); err != nil {
		return "", err
	} else if ok {
		d.log.Debug("using the cached artifact",
			slog.String("core", b.Core), slog.String("version", b.Version))
		return path, nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return "", fmt.Errorf("creating the cache directory: %w", err)
	}

	if err := d.download(ctx, b, path); err != nil {
		return "", err
	}
	return path, nil
}

// cached reports whether a usable copy is already on disk.
//
// A cached file is only trusted when its checksum still matches. Skipping the
// check would mean a truncated download, or a file corrupted on disk, is
// handed to the JVM as if it were fine — which surfaces as an unexplained
// crash on start rather than as a download error.
func (d *Downloader) cached(path string, b *Build) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking the cache: %w", err)
	}
	if info.IsDir() {
		return false, fmt.Errorf("cache path %s is a directory", path)
	}

	if b.SizeBytes > 0 && info.Size() != b.SizeBytes {
		d.log.Warn("cached artifact has the wrong size, refetching",
			slog.String("path", path),
			slog.Int64("have", info.Size()), slog.Int64("want", b.SizeBytes))
		return false, nil
	}

	if !b.Verifiable() {
		// Nothing to check it against, so an existing file of the right size
		// is as much as can be claimed.
		return true, nil
	}

	sum, err := checksumFile(path, b.Algorithm)
	if err != nil {
		return false, err
	}
	if !strings.EqualFold(sum, b.Checksum) {
		d.log.Warn("cached artifact failed its checksum, refetching",
			slog.String("path", path))
		return false, nil
	}
	return true, nil
}

// download fetches the artifact to a temporary file, verifies it and only
// then moves it into place.
//
// The temporary file matters: writing straight to the cache path would leave
// a half-written jar there if the connection dropped, and the next start
// would happily use it.
func (d *Downloader) download(ctx context.Context, b *Build, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.URL, nil)
	if err != nil {
		return fmt.Errorf("building the download request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)

	client := d.Client
	if client == nil {
		client = &http.Client{Timeout: downloadTimeout}
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", b.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading %s: unexpected status %s", b.URL, resp.Status)
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".partial-*")
	if err != nil {
		return fmt.Errorf("creating a temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath) // no-op once the rename has happened
	}()

	hasher, err := newHasher(b.Algorithm)
	if err != nil {
		return err
	}

	var written int64
	if hasher != nil {
		written, err = io.Copy(io.MultiWriter(tmp, hasher), resp.Body)
	} else {
		written, err = io.Copy(tmp, resp.Body)
	}
	if err != nil {
		return fmt.Errorf("writing the download: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing the download: %w", err)
	}

	if b.SizeBytes > 0 && written != b.SizeBytes {
		return fmt.Errorf("downloaded %d bytes, upstream declared %d", written, b.SizeBytes)
	}

	if hasher != nil {
		sum := hex.EncodeToString(hasher.Sum(nil))
		if !strings.EqualFold(sum, b.Checksum) {
			return fmt.Errorf("%w: %s %s, expected %s",
				ErrChecksum, b.Algorithm, sum, b.Checksum)
		}
	} else {
		d.log.Warn("upstream publishes no checksum for this build, integrity unverified",
			slog.String("core", b.Core), slog.String("version", b.Version))
	}

	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("moving the download into the cache: %w", err)
	}
	return nil
}

func newHasher(algorithm string) (hash.Hash, error) {
	switch strings.ToLower(algorithm) {
	case "":
		return nil, nil
	case AlgoSHA1:
		// Weak against deliberate collisions, but this is what Mojang
		// publishes; it still catches the truncation and corruption that
		// actually happen.
		return sha1.New(), nil //nolint:gosec // upstream-published algorithm
	case AlgoSHA256:
		return sha256.New(), nil
	default:
		return nil, fmt.Errorf("unsupported checksum algorithm %q", algorithm)
	}
}

func checksumFile(path, algorithm string) (string, error) {
	hasher, err := newHasher(algorithm)
	if err != nil {
		return "", err
	}
	if hasher == nil {
		return "", nil
	}

	// The file this function just downloaded, to hash it.
	file, err := os.Open(path) // #nosec G304 -- the download's own temporary file
	if err != nil {
		return "", fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	if _, err := io.Copy(hasher, file); err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// sanitizePathPart strips anything that could escape the cache directory or
// confuse a filesystem. Upstream ids are tame today, but they are remote
// input and end up in a path.
func sanitizePathPart(part string) string {
	part = strings.TrimSpace(part)
	if part == "" || part == "." || part == ".." {
		return ""
	}

	var b strings.Builder
	for _, r := range part {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '-' || r == '_' || r == '+':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}

	out := b.String()
	// A name made only of dots would still be "." or ".." after filtering.
	if strings.Trim(out, ".") == "" {
		return ""
	}
	return out
}

// getJSON fetches and decodes a JSON document from an upstream API.
func getJSON(ctx context.Context, client *http.Client, url string, target any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("building a request for %s: %w", url, err)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("requesting %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return ErrUnknownVersion
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("requesting %s: unexpected status %s", url, resp.Status)
	}

	// Bounded so a hostile or broken endpoint cannot make the daemon allocate
	// without limit. Version manifests are a few hundred kilobytes.
	body := io.LimitReader(resp.Body, 32<<20)
	if err := decodeJSON(body, target); err != nil {
		return fmt.Errorf("decoding %s: %w", url, err)
	}
	return nil
}
