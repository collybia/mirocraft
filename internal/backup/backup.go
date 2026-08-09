// Package backup archives and restores server directories.
//
// A backup is a zip rather than a tar: an operator who downloads one usually
// wants to open it, and every desktop opens a zip without installing
// anything.
package backup

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// Errors.
var (
	ErrNotFound     = errors.New("backup not found")
	ErrServerActive = errors.New("the server must be stopped first")
	ErrTooLarge     = errors.New("the backup is too large")
)

// MaxEntries caps how many files a backup or restore will process.
const MaxEntries = 200_000

// excluded names are skipped when archiving.
//
// They are all re-downloadable: the core jar, the libraries and version
// metadata a modern server unpacks beside it, and the caches. Including them
// would turn a 20 MB world backup into a 250 MB one that takes minutes, and
// restoring is no worse off because a start re-provisions all of it.
var excluded = map[string]struct{}{
	"server.jar":    {},
	"libraries":     {},
	"versions":      {},
	"cache":         {},
	"bundler":       {},
	".fabric":       {},
	"crash-reports": {},
}

// Manager creates and restores backups.
type Manager struct {
	// Dir is where archives are stored, one subdirectory per server.
	Dir string

	log *slog.Logger
}

// NewManager returns a manager storing archives under dir.
func NewManager(dir string, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{Dir: dir, log: log}
}

// Path is where a backup's archive lives.
func (m *Manager) Path(serverID, backupID string) string {
	return filepath.Join(m.Dir, sanitize(serverID), sanitize(backupID)+".zip")
}

// Create archives a server directory and returns the archive path and size.
//
// Files that cannot be read are skipped rather than failing the run. A live
// Minecraft server holds world/session.lock open, and on Windows that makes
// it unreadable outright — refusing to back up a running server at all would
// be a much worse answer than a backup without a lock file, which is not
// data and should not be restored anyway.
func (m *Manager) Create(ctx context.Context, serverID, backupID, serverDir string) (string, int64, error) {
	target := m.Path(serverID, backupID)
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return "", 0, fmt.Errorf("creating the backup directory: %w", err)
	}

	// Written to a temporary file and moved into place, so an interrupted run
	// never leaves a truncated archive that looks like a usable backup.
	tmp, err := os.CreateTemp(filepath.Dir(target), ".backup-*")
	if err != nil {
		return "", 0, fmt.Errorf("creating a temporary archive: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath) // a no-op once renamed
	}()

	writer := zip.NewWriter(tmp)
	entries := 0
	skipped := 0

	err = filepath.Walk(serverDir, func(p string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}

		rel, err := filepath.Rel(serverDir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		name := filepath.ToSlash(rel)

		if isExcluded(name) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		entries++
		if entries > MaxEntries {
			return fmt.Errorf("%w: more than %d files", ErrTooLarge, MaxEntries)
		}

		if info.IsDir() {
			_, err := writer.Create(name + "/")
			return err
		}
		// Symlinks are skipped rather than followed: following one would copy
		// a file from outside the server directory into the archive.
		if info.Mode()&os.ModeSymlink != 0 {
			return nil
		}

		// Walking a directory this manager was given; the path is not
		// assembled from a request.
		// Symlinks are skipped a few lines above, so the walk cannot be led
		// out of the tree between the stat and this open.
		src, err := os.Open(p) // #nosec G304,G122 -- a walk path, symlinks already skipped
		if err != nil {
			// A running server rewrites files as it goes and holds some open;
			// one unreadable file is not worth losing the whole backup for.
			entries--
			skipped++
			m.log.Warn("skipping an unreadable file",
				slog.String("path", name), slog.String("error", err.Error()))
			return nil
		}
		defer func() { _ = src.Close() }()

		w, err := writer.Create(name)
		if err != nil {
			return err
		}

		if _, err := io.Copy(w, src); err != nil {
			// Half a file in the archive is worse than none, but the entry
			// header is already written, so the run continues and the count
			// records it.
			skipped++
			m.log.Warn("skipping a file that could not be read fully",
				slog.String("path", name), slog.String("error", err.Error()))
		}
		return nil
	})
	if err != nil {
		_ = writer.Close()
		return "", 0, fmt.Errorf("archiving %s: %w", serverDir, err)
	}

	if err := writer.Close(); err != nil {
		return "", 0, fmt.Errorf("finishing the archive: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", 0, fmt.Errorf("closing the archive: %w", err)
	}

	info, err := os.Stat(tmpPath)
	if err != nil {
		return "", 0, fmt.Errorf("reading the archive: %w", err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return "", 0, fmt.Errorf("storing the archive: %w", err)
	}

	m.log.Info("backup created",
		slog.String("server_id", serverID), slog.String("backup_id", backupID),
		slog.Int("files", entries), slog.Int("skipped", skipped),
		slog.Int64("bytes", info.Size()))

	return target, info.Size(), nil
}

// Restore unpacks an archive over a server directory.
//
// Everything the archive did not carry is cleared first, so restoring really
// returns the server to the state it was in rather than merging with whatever
// is there now — a world with leftover region files from a later session is
// worse than either version. The excluded artifacts are kept, since they are
// re-provisioned rather than backed up.
func (m *Manager) Restore(ctx context.Context, archivePath, serverDir string) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("opening the backup: %w", err)
	}
	defer func() { _ = reader.Close() }()

	if len(reader.File) > MaxEntries {
		return fmt.Errorf("%w: more than %d files", ErrTooLarge, MaxEntries)
	}

	if err := clearDirectory(serverDir); err != nil {
		return err
	}

	for _, entry := range reader.File {
		if err := ctx.Err(); err != nil {
			return err
		}

		target, err := safeJoin(serverDir, entry.Name)
		if err != nil {
			return fmt.Errorf("backup entry %q: %w", entry.Name, err)
		}

		info := entry.FileInfo()
		if info.IsDir() {
			if err := os.MkdirAll(target, 0o750); err != nil {
				return fmt.Errorf("creating %s: %w", entry.Name, err)
			}
			continue
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return fmt.Errorf("creating the parent of %s: %w", entry.Name, err)
		}

		src, err := entry.Open()
		if err != nil {
			return fmt.Errorf("reading %s: %w", entry.Name, err)
		}
		err = writeFile(target, src)
		_ = src.Close()
		if err != nil {
			return err
		}
	}

	m.log.Info("backup restored",
		slog.String("archive", archivePath), slog.String("dir", serverDir))
	return nil
}

// Delete removes an archive.
func (m *Manager) Delete(serverID, backupID string) error {
	path := m.Path(serverID, backupID)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("deleting %s: %w", path, err)
	}
	return nil
}

// Open returns a reader for downloading an archive.
func (m *Manager) Open(serverID, backupID string) (*os.File, os.FileInfo, error) {
	p := m.Path(serverID, backupID)

	// Path sanitises both ids before joining them.
	file, err := os.Open(p) // #nosec G304 -- built by Path from sanitised ids
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, fmt.Errorf("opening %s: %w", p, err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("reading %s: %w", p, err)
	}
	return file, info, nil
}

// SuggestName is the file name offered when downloading.
func SuggestName(serverName string, at time.Time) string {
	clean := sanitize(serverName)
	if clean == "" {
		clean = "server"
	}
	return fmt.Sprintf("%s-%s.zip", clean, at.UTC().Format("2006-01-02-1504"))
}

// --- helpers ---

// isExcluded reports whether a path inside the server directory is skipped.
// Only the top level is matched: a world folder called "cache" deeper in the
// tree is the operator's data and gets backed up.
func isExcluded(name string) bool {
	top := name
	if i := strings.IndexByte(name, '/'); i >= 0 {
		top = name[:i]
	}
	_, found := excluded[top]
	return found
}

// clearDirectory empties a server directory, keeping the artifacts that
// backups deliberately do not carry.
func clearDirectory(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return os.MkdirAll(dir, 0o750)
		}
		return fmt.Errorf("reading %s: %w", dir, err)
	}

	for _, entry := range entries {
		if isExcluded(entry.Name()) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, entry.Name())); err != nil {
			return fmt.Errorf("clearing %s: %w", entry.Name(), err)
		}
	}
	return nil
}

// safeJoin resolves an archive member against the destination, refusing one
// that would land outside it.
//
// A backup archive is usually one the panel wrote, but it is also a file an
// operator can replace on disk, so it is treated as untrusted input like any
// other archive.
func safeJoin(dir, name string) (string, error) {
	clean := path.Clean("/" + strings.ReplaceAll(name, "\\", "/"))
	clean = strings.TrimPrefix(clean, "/")
	if clean == "" || clean == "." {
		return "", errors.New("the entry has no name")
	}

	for _, part := range strings.Split(clean, "/") {
		if part == ".." {
			return "", errors.New("the entry escapes the destination")
		}
	}

	target := filepath.Join(dir, filepath.FromSlash(clean))
	rel, err := filepath.Rel(dir, target)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("the entry escapes the destination")
	}
	return target, nil
}

func writeFile(target string, src io.Reader) error {
	// target comes from safeTarget above, which refuses anything that leaves
	// the destination directory.
	file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640) // #nosec G304 -- checked by safeTarget
	if err != nil {
		return fmt.Errorf("creating %s: %w", target, err)
	}
	if _, err := io.Copy(file, src); err != nil {
		_ = file.Close()
		return fmt.Errorf("writing %s: %w", target, err)
	}
	return file.Close()
}

// sanitize strips anything that could escape a directory or upset a
// filesystem, since ids and names end up in paths.
func sanitize(part string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(part) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}
