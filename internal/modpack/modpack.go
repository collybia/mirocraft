// Package modpack installs a Modrinth modpack into a server directory.
//
// A .mrpack is a zip holding an index and a tree of overrides. The index names
// every mod by URL and hash rather than shipping it — redistributing other
// people's mods is what modpack authors are not allowed to do — so installing
// one means downloading a hundred-odd files and putting them where the index
// says.
//
// CurseForge packs are not here. Their API requires a key on every download,
// and a self-hosted panel has no key to hand every operator; a pack format
// that cannot be fetched is worse than one the panel says plainly it does not
// support.
package modpack

import (
	"archive/zip"
	"context"
	"crypto/sha1" //nolint:gosec // Modrinth publishes sha1 digests; verifying against them is the point
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// IndexName is the document inside a .mrpack that describes it.
const IndexName = "modrinth.index.json"

// The directories a pack copies wholesale into the instance.
const (
	overridesDir       = "overrides/"
	serverOverridesDir = "server-overrides/"
	clientOverridesDir = "client-overrides/"
)

// allowedHosts are the only places a pack may point a download at.
//
// Modrinth's own rule, and worth enforcing rather than trusting: the index is
// written by whoever published the pack, and without this it is a list of
// URLs that makes the panel fetch anything from anywhere — including addresses
// only reachable from inside the operator's network.
var allowedHosts = map[string]bool{
	"cdn.modrinth.com":          true,
	"github.com":                true,
	"raw.githubusercontent.com": true,
	"gitlab.com":                true,
}

// Limits on what a pack may ask for. A modpack is a few hundred megabytes
// across a few hundred files; these are far above that and far below anything
// that would fill a disk.
const (
	MaxFiles     = 2000
	MaxFileBytes = 512 << 20
	MaxTotal     = 8 << 30
	// MaxOverrides caps the files copied out of the archive itself.
	MaxOverrides = 20_000
)

// downloadTimeout bounds one file.
const downloadTimeout = 10 * time.Minute

// concurrency is how many files are fetched at once.
//
// Four rather than one because a pack is a hundred small files and serial
// fetching is dominated by round trips; four rather than forty because the
// other end is a free CDN and a panel is not entitled to saturate it.
const concurrency = 4

// userAgent identifies the panel.
const userAgent = "Mirocraft/1.0 (+https://github.com/collybia/mirocraft)"

// Errors.
var (
	ErrNotAPack    = errors.New("this file is not a Modrinth modpack")
	ErrUnsupported = errors.New("this pack asks for something the panel cannot provide")
	ErrDisallowed  = errors.New("the pack points a download somewhere it may not")
	ErrTooLarge    = errors.New("the pack is larger than the panel will install")
	ErrChecksum    = errors.New("a downloaded file failed its checksum")
)

// Index is the pack's own description of itself.
type Index struct {
	FormatVersion int    `json:"formatVersion"`
	Game          string `json:"game"`
	VersionID     string `json:"versionId"`
	Name          string `json:"name"`
	Summary       string `json:"summary"`

	Files []File `json:"files"`
	// Dependencies names the loader and the Minecraft version, for example
	// {"minecraft": "1.21.4", "fabric-loader": "0.16.9"}.
	Dependencies map[string]string `json:"dependencies"`
}

// File is one download the pack asks for.
type File struct {
	Path      string            `json:"path"`
	Hashes    map[string]string `json:"hashes"`
	Env       map[string]string `json:"env"`
	Downloads []string          `json:"downloads"`
	FileSize  int64             `json:"fileSize"`
}

// WantedOnServer reports whether this file belongs on a server.
//
// A modpack carries client-only mods — shaders, menus, minimaps — and putting
// them on a server is how a server fails to start with a stack trace about a
// missing client class.
func (f File) WantedOnServer() bool {
	if f.Env == nil {
		return true
	}
	return f.Env["server"] != "unsupported"
}

// Loader is what a pack needs to run.
type Loader struct {
	// Core is the panel's core id: fabric, quilt, forge, neoforge.
	Core string
	// Minecraft is the version the pack is for.
	Minecraft string
	// Version is the loader's own version, where the pack pins one.
	Version string
}

// loaderKeys maps the dependency names Modrinth uses onto the panel's cores.
var loaderKeys = map[string]string{
	"fabric-loader": "fabric",
	"quilt-loader":  "quilt",
	"forge":         "forge",
	"neoforge":      "neoforge",
}

// loaderCores maps the loader names Modrinth publishes on a version onto the
// panel's cores. The same information as loaderKeys, from the other document:
// the index names "fabric-loader", the version listing says "fabric".
var loaderCores = map[string]string{
	"fabric":   "fabric",
	"quilt":    "quilt",
	"forge":    "forge",
	"neoforge": "neoforge",
}

// CoreForLoader maps a Modrinth loader name onto the core that runs it.
//
// For the preview: a pack's loader can be read off the version listing without
// downloading the pack, so the panel can say what one click will change before
// it changes it. The index is still what the install itself goes by.
func CoreForLoader(name string) (string, bool) {
	core, ok := loaderCores[strings.ToLower(strings.TrimSpace(name))]
	return core, ok
}

// LoaderFor works out which core a pack needs.
func (i *Index) LoaderFor() (Loader, error) {
	minecraft := i.Dependencies["minecraft"]
	if minecraft == "" {
		return Loader{}, fmt.Errorf("%w: it names no Minecraft version", ErrNotAPack)
	}

	for key, core := range loaderKeys {
		if version, ok := i.Dependencies[key]; ok {
			return Loader{Core: core, Minecraft: minecraft, Version: version}, nil
		}
	}
	return Loader{}, fmt.Errorf("%w: %v names no loader the panel installs", ErrUnsupported, i.Dependencies)
}

// Installer puts a pack into a server directory.
type Installer struct {
	// Client fetches the files. Nil uses one with a sensible timeout.
	Client *http.Client
	Log    *slog.Logger
}

// Progress reports how far an install has got, for the task the panel polls.
type Progress func(done, total int)

// ReadIndex opens a .mrpack and returns its index.
func ReadIndex(path string) (*Index, error) {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotAPack, err)
	}
	defer func() { _ = reader.Close() }()

	return readIndex(&reader.Reader)
}

func readIndex(reader *zip.Reader) (*Index, error) {
	for _, entry := range reader.File {
		if entry.Name != IndexName {
			continue
		}
		body, err := entry.Open()
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", IndexName, err)
		}
		defer func() { _ = body.Close() }()

		var index Index
		if err := json.NewDecoder(io.LimitReader(body, 32<<20)).Decode(&index); err != nil {
			return nil, fmt.Errorf("%w: %s does not parse: %w", ErrNotAPack, IndexName, err)
		}
		if index.Game != "" && index.Game != "minecraft" {
			return nil, fmt.Errorf("%w: it is for %q rather than minecraft", ErrUnsupported, index.Game)
		}
		if len(index.Files) > MaxFiles {
			return nil, fmt.Errorf("%w: %d files", ErrTooLarge, len(index.Files))
		}
		return &index, nil
	}
	return nil, fmt.Errorf("%w: it has no %s", ErrNotAPack, IndexName)
}

// Install unpacks a pack into dir: the overrides from the archive, then every
// file the index names.
func (in *Installer) Install(ctx context.Context, packPath, dir string, progress Progress) (*Index, error) {
	reader, err := zip.OpenReader(packPath)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotAPack, err)
	}
	defer func() { _ = reader.Close() }()

	index, err := readIndex(&reader.Reader)
	if err != nil {
		return nil, err
	}

	if err := in.applyOverrides(&reader.Reader, dir); err != nil {
		return nil, err
	}
	if err := in.fetchFiles(ctx, index, dir, progress); err != nil {
		return nil, err
	}
	return index, nil
}

// applyOverrides copies the archive's own files into the server directory.
//
// server-overrides wins over overrides where both carry the same path: that is
// what the format is for, and a pack that ships a client config and a server
// config for the same file means the second one.
func (in *Installer) applyOverrides(reader *zip.Reader, dir string) error {
	copied := 0
	for _, pass := range []string{overridesDir, serverOverridesDir} {
		for _, entry := range reader.File {
			if !strings.HasPrefix(entry.Name, pass) {
				continue
			}
			// Client overrides are for the launcher, not for a server.
			if strings.HasPrefix(entry.Name, clientOverridesDir) {
				continue
			}
			relative := strings.TrimPrefix(entry.Name, pass)
			if relative == "" {
				continue
			}

			copied++
			if copied > MaxOverrides {
				return fmt.Errorf("%w: more than %d override files", ErrTooLarge, MaxOverrides)
			}
			if err := extractOverride(entry, dir, relative); err != nil {
				return err
			}
		}
	}
	return nil
}

// fetchFiles downloads everything the index names.
func (in *Installer) fetchFiles(ctx context.Context, index *Index, dir string, progress Progress) error {
	wanted := make([]File, 0, len(index.Files))
	var total int64
	for _, file := range index.Files {
		if !file.WantedOnServer() {
			continue
		}
		total += file.FileSize
		wanted = append(wanted, file)
	}
	if total > MaxTotal {
		return fmt.Errorf("%w: %d bytes of downloads", ErrTooLarge, total)
	}

	var (
		mu     sync.Mutex
		done   int
		failed error
	)
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	for _, file := range wanted {
		wg.Add(1)
		go func(file File) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			mu.Lock()
			stop := failed != nil
			mu.Unlock()
			if stop {
				return
			}

			if err := in.fetchOne(ctx, file, dir); err != nil {
				mu.Lock()
				if failed == nil {
					failed = err
				}
				mu.Unlock()
				return
			}

			mu.Lock()
			done++
			at := done
			mu.Unlock()
			if progress != nil {
				progress(at, len(wanted))
			}
		}(file)
	}
	wg.Wait()

	return failed
}

// fetchOne downloads one file and checks it against the hash the pack states.
func (in *Installer) fetchOne(ctx context.Context, file File, dir string) error {
	target, err := SafePath(dir, file.Path)
	if err != nil {
		return err
	}
	if file.FileSize > MaxFileBytes {
		return fmt.Errorf("%w: %s is %d bytes", ErrTooLarge, file.Path, file.FileSize)
	}
	if len(file.Downloads) == 0 {
		return fmt.Errorf("%w: %s names no download", ErrNotAPack, file.Path)
	}

	// Already there and matching: an install re-run after a failure should not
	// fetch what it already has.
	if ok, _ := matchesHash(target, file.Hashes); ok {
		return nil
	}

	var lastErr error
	for _, raw := range file.Downloads {
		if err := checkHost(raw); err != nil {
			lastErr = err
			continue
		}
		if err := in.download(ctx, raw, target, file); err != nil {
			lastErr = err
			continue
		}
		return nil
	}
	return fmt.Errorf("downloading %s: %w", file.Path, lastErr)
}

// checkHost refuses a download the pack should not be asking for.
func checkHost(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%w: %q is not a url", ErrDisallowed, raw)
	}
	if parsed.Scheme != "https" {
		return fmt.Errorf("%w: %s is not https", ErrDisallowed, parsed.Scheme)
	}
	if !allowedHosts[strings.ToLower(parsed.Hostname())] {
		return fmt.Errorf("%w: %s", ErrDisallowed, parsed.Hostname())
	}
	return nil
}

// download fetches one file, verifying it as it is written.
func (in *Installer) download(ctx context.Context, raw, target string, file File) error {
	fetchCtx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, raw, nil)
	if err != nil {
		return fmt.Errorf("building the request: %w", err)
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := in.client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %s", resp.Status)
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(target), err)
	}
	// Written beside the target and moved, so an interrupted download never
	// leaves a truncated mod that the server then fails to load.
	temp, err := os.CreateTemp(filepath.Dir(target), ".downloading-*")
	if err != nil {
		return fmt.Errorf("creating a temporary file: %w", err)
	}
	tempPath := temp.Name()
	defer func() {
		_ = temp.Close()
		_ = os.Remove(tempPath)
	}()

	hasher, algorithm := newHasher(file.Hashes)
	var writer io.Writer = temp
	if hasher != nil {
		writer = io.MultiWriter(temp, hasher)
	}

	if _, err := io.Copy(writer, io.LimitReader(resp.Body, MaxFileBytes+1)); err != nil {
		return fmt.Errorf("writing %s: %w", target, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tempPath, err)
	}

	if hasher != nil {
		got := hex.EncodeToString(hasher.Sum(nil))
		if !strings.EqualFold(got, file.Hashes[algorithm]) {
			return fmt.Errorf("%w: %s", ErrChecksum, file.Path)
		}
	}

	if err := os.Rename(tempPath, target); err != nil {
		return fmt.Errorf("moving %s into place: %w", target, err)
	}
	return nil
}

// newHasher picks the strongest hash the pack published.
func newHasher(hashes map[string]string) (hash.Hash, string) {
	if hashes["sha512"] != "" {
		return sha512.New(), "sha512"
	}
	if hashes["sha1"] != "" {
		// Weak against a deliberate collision and still what Modrinth
		// publishes; it catches the truncation and corruption that happen.
		return sha1.New(), "sha1" //nolint:gosec // upstream-published algorithm
	}
	return nil, ""
}

// matchesHash reports whether a file already on disk matches.
func matchesHash(path string, hashes map[string]string) (bool, error) {
	hasher, algorithm := newHasher(hashes)
	if hasher == nil {
		return false, nil
	}

	// A path this package built from the server's directory.
	file, err := os.Open(path) // #nosec G304 -- confined by SafePath
	if err != nil {
		return false, err
	}
	defer func() { _ = file.Close() }()

	if _, err := io.Copy(hasher, file); err != nil {
		return false, err
	}
	return strings.EqualFold(hex.EncodeToString(hasher.Sum(nil)), hashes[algorithm]), nil
}

func (in *Installer) client() *http.Client {
	if in.Client != nil {
		return in.Client
	}
	return &http.Client{Timeout: downloadTimeout}
}
