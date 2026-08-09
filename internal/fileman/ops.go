package fileman

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Limits. They exist so one request cannot exhaust the host.
const (
	// MaxTextBytes is the largest file the text editor will load or save.
	MaxTextBytes = 2 << 20 // 2 MiB
	// MaxUploadBytes is the largest single upload.
	MaxUploadBytes = 1 << 30 // 1 GiB
	// MaxListEntries caps a directory listing.
	MaxListEntries = 5000
	// MaxArchiveEntries caps what an archive operation will process.
	MaxArchiveEntries = 100_000
	// MaxArchiveBytes caps what an extraction may write in total.
	//
	// The per-entry limit alone is not one: a hundred thousand entries of a
	// gigabyte each is the entry limit multiplied by the file limit, and a
	// zip that small on disk expanding to that much is the whole idea behind
	// a decompression bomb. Eight gigabytes is far more than any world or
	// modpack anyone extracts through a panel, and far less than a disk.
	MaxArchiveBytes = 8 << 30 // 8 GiB
)

// archiveBudget is MaxArchiveBytes, as a variable so a test can exercise the
// refusal without writing eight gigabytes to prove it.
var archiveBudget int64 = MaxArchiveBytes

// Operation errors.
var (
	ErrTooLarge     = errors.New("file is too large")
	ErrNotADirEntry = errors.New("path is not a directory")
	ErrIsADirectory = errors.New("path is a directory")
	ErrExists       = errors.New("path already exists")
	ErrNotEmpty     = errors.New("directory is not empty")
	ErrBinary       = errors.New("file is not text")
)

// Entry is one item in a directory listing.
type Entry struct {
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	Type       string    `json:"type"` // file | directory | symlink
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at"`
	Mode       string    `json:"mode"`
}

// Entry types.
const (
	TypeFile      = "file"
	TypeDirectory = "directory"
	TypeSymlink   = "symlink"
)

// List returns the contents of a directory, directories first and then by
// name, which is what a file manager shows.
func (r *Root) List(rel string) ([]Entry, error) {
	abs, err := r.ResolveExisting(rel)
	if err != nil {
		return nil, err
	}

	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", rel, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: %s", ErrNotADirEntry, rel)
	}

	dirEntries, err := os.ReadDir(abs)
	if err != nil {
		return nil, fmt.Errorf("listing %s: %w", rel, err)
	}
	if len(dirEntries) > MaxListEntries {
		dirEntries = dirEntries[:MaxListEntries]
	}

	out := make([]Entry, 0, len(dirEntries))
	for _, entry := range dirEntries {
		item, err := r.describe(abs, entry.Name())
		if err != nil {
			// A file that vanished between the listing and the stat is not
			// worth failing the whole request over.
			continue
		}
		out = append(out, item)
	}

	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].Type == TypeDirectory) != (out[j].Type == TypeDirectory) {
			return out[i].Type == TypeDirectory
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

// describe builds a listing entry for one name inside a directory.
func (r *Root) describe(dir, name string) (Entry, error) {
	full := filepath.Join(dir, name)

	// Lstat, not Stat: a symlink is reported as a symlink rather than as
	// whatever it points at, so the panel can show that it is one and a
	// listing cannot be made to describe a file outside the root.
	info, err := os.Lstat(full)
	if err != nil {
		return Entry{}, err
	}

	relPath, err := r.Rel(full)
	if err != nil {
		return Entry{}, err
	}

	entry := Entry{
		Name:       name,
		Path:       relPath,
		Size:       info.Size(),
		ModifiedAt: info.ModTime().UTC(),
		Mode:       info.Mode().Perm().String(),
	}

	switch {
	case info.Mode()&os.ModeSymlink != 0:
		entry.Type = TypeSymlink
	case info.IsDir():
		entry.Type = TypeDirectory
	default:
		entry.Type = TypeFile
	}
	return entry, nil
}

// Stat describes a single path.
func (r *Root) Stat(rel string) (Entry, error) {
	abs, err := r.ResolveExisting(rel)
	if err != nil {
		return Entry{}, err
	}
	return r.describe(filepath.Dir(abs), filepath.Base(abs))
}

// ReadText reads a file for the editor, refusing anything too large or not
// text.
//
// The binary check is not squeamishness: handing a jar to a text editor
// produces a screenful of replacement characters and, worse, saving it back
// would corrupt the file.
func (r *Root) ReadText(rel string) (string, error) {
	abs, err := r.ResolveExisting(rel)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", rel, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%w: %s", ErrIsADirectory, rel)
	}
	if info.Size() > MaxTextBytes {
		return "", fmt.Errorf("%w: %s is %d bytes, the editor limit is %d",
			ErrTooLarge, rel, info.Size(), MaxTextBytes)
	}

	// abs comes from Root.Resolve, this package's guard against leaving the
	// server's directory; escaping it is what fileman's tests are about.
	body, err := os.ReadFile(abs) // #nosec G304 -- confined by Root.Resolve
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", rel, err)
	}
	if isBinary(body) {
		return "", fmt.Errorf("%w: %s", ErrBinary, rel)
	}
	return string(body), nil
}

// WriteText replaces a file's contents.
func (r *Root) WriteText(rel, content string) error {
	if len(content) > MaxTextBytes {
		return fmt.Errorf("%w: %d bytes, the editor limit is %d",
			ErrTooLarge, len(content), MaxTextBytes)
	}

	abs, err := r.Resolve(rel)
	if err != nil {
		return err
	}
	if info, err := os.Stat(abs); err == nil && info.IsDir() {
		return fmt.Errorf("%w: %s", ErrIsADirectory, rel)
	}

	return writeAtomic(abs, strings.NewReader(content), int64(len(content)))
}

// Open returns a reader for downloading a file.
func (r *Root) Open(rel string) (*os.File, os.FileInfo, error) {
	abs, err := r.ResolveExisting(rel)
	if err != nil {
		return nil, nil, err
	}

	info, err := os.Stat(abs)
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", rel, err)
	}
	if info.IsDir() {
		return nil, nil, fmt.Errorf("%w: %s", ErrIsADirectory, rel)
	}

	// abs comes from Root.Resolve, this package's guard against leaving the
	// server's directory; escaping it is what fileman's tests are about.
	file, err := os.Open(abs) // #nosec G304 -- confined by Root.Resolve
	if err != nil {
		return nil, nil, fmt.Errorf("opening %s: %w", rel, err)
	}
	return file, info, nil
}

// Upload writes a stream to a path, refusing more than the limit.
func (r *Root) Upload(rel string, body io.Reader) (int64, error) {
	abs, err := r.Resolve(rel)
	if err != nil {
		return 0, err
	}
	if info, err := os.Stat(abs); err == nil && info.IsDir() {
		return 0, fmt.Errorf("%w: %s", ErrIsADirectory, rel)
	}

	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		return 0, fmt.Errorf("creating the parent of %s: %w", rel, err)
	}

	// One byte past the limit is read on purpose, so exceeding it is
	// detected rather than silently truncated.
	limited := io.LimitReader(body, MaxUploadBytes+1)

	written, err := writeAtomicCounted(abs, limited)
	if err != nil {
		return 0, err
	}
	if written > MaxUploadBytes {
		_ = os.Remove(abs)
		return 0, fmt.Errorf("%w: uploads are limited to %d bytes", ErrTooLarge, MaxUploadBytes)
	}
	return written, nil
}

// Mkdir creates a directory and any missing parents.
func (r *Root) Mkdir(rel string) error {
	abs, err := r.Resolve(rel)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(abs); err == nil {
		return fmt.Errorf("%w: %s", ErrExists, rel)
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", rel, err)
	}
	return nil
}

// Remove deletes a path, recursively for directories.
func (r *Root) Remove(rel string) error {
	abs, err := r.ResolveExisting(rel)
	if err != nil {
		return err
	}

	// Deleting the root itself would leave a server with no directory at all.
	if abs == r.dir {
		return fmt.Errorf("%w: the server directory itself cannot be deleted", ErrInvalidPath)
	}

	if err := os.RemoveAll(abs); err != nil {
		return fmt.Errorf("deleting %s: %w", rel, err)
	}
	return nil
}

// Move renames a path. Both ends are resolved, so neither can point outside.
func (r *Root) Move(fromRel, toRel string) error {
	from, err := r.ResolveExisting(fromRel)
	if err != nil {
		return err
	}
	to, err := r.Resolve(toRel)
	if err != nil {
		return err
	}

	if from == r.dir {
		return fmt.Errorf("%w: the server directory itself cannot be moved", ErrInvalidPath)
	}
	if _, err := os.Lstat(to); err == nil {
		return fmt.Errorf("%w: %s", ErrExists, toRel)
	}

	// Moving a directory into itself produces a detached subtree that nothing
	// can reach, and the operating system does not always stop it.
	if isUnder(to, from) {
		return fmt.Errorf("%w: cannot move %s into itself", ErrInvalidPath, fromRel)
	}

	if err := os.MkdirAll(filepath.Dir(to), 0o750); err != nil {
		return fmt.Errorf("creating the parent of %s: %w", toRel, err)
	}
	if err := os.Rename(from, to); err != nil {
		return fmt.Errorf("moving %s to %s: %w", fromRel, toRel, err)
	}
	return nil
}

// Copy duplicates a file or a directory tree.
func (r *Root) Copy(fromRel, toRel string) error {
	from, err := r.ResolveExisting(fromRel)
	if err != nil {
		return err
	}
	to, err := r.Resolve(toRel)
	if err != nil {
		return err
	}

	if _, err := os.Lstat(to); err == nil {
		return fmt.Errorf("%w: %s", ErrExists, toRel)
	}
	if isUnder(to, from) {
		return fmt.Errorf("%w: cannot copy %s into itself", ErrInvalidPath, fromRel)
	}

	info, err := os.Stat(from)
	if err != nil {
		return fmt.Errorf("reading %s: %w", fromRel, err)
	}

	if info.IsDir() {
		return copyTree(from, to)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o750); err != nil {
		return fmt.Errorf("creating the parent of %s: %w", toRel, err)
	}
	return copyOneFile(from, to)
}

// Archive packs the given paths into a zip inside the root and returns its
// path relative to the root.
func (r *Root) Archive(paths []string, destRel string) (string, error) {
	if len(paths) == 0 {
		return "", fmt.Errorf("%w: nothing to archive", ErrInvalidPath)
	}

	dest, err := r.Resolve(destRel)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(dest); err == nil {
		return "", fmt.Errorf("%w: %s", ErrExists, destRel)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return "", fmt.Errorf("creating the parent of %s: %w", destRel, err)
	}

	// dest comes from Root.Resolve, like every other path here.
	file, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640) // #nosec G304 -- confined by Root.Resolve
	if err != nil {
		return "", fmt.Errorf("creating %s: %w", destRel, err)
	}
	defer func() { _ = file.Close() }()

	writer := zip.NewWriter(file)
	entries := 0

	for _, rel := range paths {
		abs, err := r.ResolveExisting(rel)
		if err != nil {
			_ = writer.Close()
			_ = os.Remove(dest)
			return "", err
		}
		// The archive being written must not end up inside itself.
		if abs == dest {
			continue
		}

		base := filepath.Dir(abs)
		err = filepath.Walk(abs, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if p == dest {
				return nil
			}
			entries++
			if entries > MaxArchiveEntries {
				return fmt.Errorf("archive would hold more than %d entries", MaxArchiveEntries)
			}

			name, err := filepath.Rel(base, p)
			if err != nil {
				return err
			}
			name = filepath.ToSlash(name)

			if info.IsDir() {
				_, err := writer.Create(name + "/")
				return err
			}
			// Symlinks are skipped rather than followed: following one would
			// pull a file from outside the root into the archive.
			if info.Mode()&os.ModeSymlink != 0 {
				return nil
			}

			w, err := writer.Create(name)
			if err != nil {
				return err
			}
			// A path produced by walking a directory Root.Resolve returned, and
			// symlinks are skipped a few lines above, so the walk cannot be
			// led out of the root between the stat and this open.
			src, err := os.Open(p) // #nosec G304,G122 -- inside the confined root, symlinks skipped
			if err != nil {
				return err
			}
			defer func() { _ = src.Close() }()
			_, err = io.Copy(w, src)
			return err
		})
		if err != nil {
			_ = writer.Close()
			_ = os.Remove(dest)
			return "", fmt.Errorf("archiving %s: %w", rel, err)
		}
	}

	if err := writer.Close(); err != nil {
		_ = os.Remove(dest)
		return "", fmt.Errorf("finishing %s: %w", destRel, err)
	}
	return r.Rel(dest)
}

// Unarchive extracts a zip or tar.gz into a directory inside the root.
//
// Every member is resolved through the same sandbox as any other path, so an
// archive uploaded by a client cannot write outside the server directory —
// the Zip Slip case, which is the whole reason unarchiving is dangerous.
func (r *Root) Unarchive(archiveRel, destRel string) error {
	archive, err := r.ResolveExisting(archiveRel)
	if err != nil {
		return err
	}
	destAbs, err := r.Resolve(destRel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(destAbs, 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", destRel, err)
	}

	destPrefix, err := r.Rel(destAbs)
	if err != nil {
		return err
	}

	switch {
	case strings.HasSuffix(strings.ToLower(archive), ".zip"):
		return r.unzip(archive, destPrefix)
	case strings.HasSuffix(strings.ToLower(archive), ".tar.gz"),
		strings.HasSuffix(strings.ToLower(archive), ".tgz"):
		return r.untar(archive, destPrefix)
	default:
		return fmt.Errorf("%w: only .zip and .tar.gz can be extracted", ErrInvalidPath)
	}
}

func (r *Root) unzip(archive, destPrefix string) error {
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("opening the archive: %w", err)
	}
	defer func() { _ = reader.Close() }()

	if len(reader.File) > MaxArchiveEntries {
		return fmt.Errorf("the archive holds more than %d entries", MaxArchiveEntries)
	}

	budget := archiveBudget
	for _, entry := range reader.File {
		target, err := r.resolveMember(destPrefix, entry.Name)
		if err != nil {
			return fmt.Errorf("archive entry %q: %w", entry.Name, err)
		}

		info := entry.FileInfo()
		if info.IsDir() {
			if err := os.MkdirAll(target, 0o750); err != nil {
				return fmt.Errorf("creating %s: %w", entry.Name, err)
			}
			continue
		}
		if entry.UncompressedSize64 > MaxUploadBytes {
			return fmt.Errorf("%w: archive entry %q", ErrTooLarge, entry.Name)
		}

		if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
			return fmt.Errorf("creating the parent of %s: %w", entry.Name, err)
		}
		body, err := entry.Open()
		if err != nil {
			return fmt.Errorf("reading %s: %w", entry.Name, err)
		}
		// Counted from what was actually written rather than from the
		// declared size: a header claiming one byte per entry is how a bomb
		// gets past a budget that trusts it.
		written, err := writeAtomicCounted(target, io.LimitReader(body, MaxUploadBytes))
		_ = body.Close()
		if err != nil {
			return err
		}
		budget -= written
		if budget < 0 {
			return fmt.Errorf("%w: the archive expands to more than %d bytes",
				ErrTooLarge, archiveBudget)
		}
	}
	return nil
}

func (r *Root) untar(archive, destPrefix string) error {
	// archive was resolved by the caller.
	file, err := os.Open(archive) // #nosec G304 -- confined by Root.Resolve
	if err != nil {
		return fmt.Errorf("opening the archive: %w", err)
	}
	defer func() { _ = file.Close() }()

	gz, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("reading the archive as gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()

	reader := tar.NewReader(gz)
	entries := 0
	budget := archiveBudget

	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading the archive: %w", err)
		}

		entries++
		if entries > MaxArchiveEntries {
			return fmt.Errorf("the archive holds more than %d entries", MaxArchiveEntries)
		}

		target, err := r.resolveMember(destPrefix, header.Name)
		if err != nil {
			return fmt.Errorf("archive entry %q: %w", header.Name, err)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				return fmt.Errorf("creating %s: %w", header.Name, err)
			}
		case tar.TypeReg:
			if header.Size > MaxUploadBytes {
				return fmt.Errorf("%w: archive entry %q", ErrTooLarge, header.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
				return fmt.Errorf("creating the parent of %s: %w", header.Name, err)
			}
			written, err := writeAtomicCounted(target, io.LimitReader(reader, MaxUploadBytes))
			if err != nil {
				return err
			}
			budget -= written
			if budget < 0 {
				return fmt.Errorf("%w: the archive expands to more than %d bytes",
					ErrTooLarge, archiveBudget)
			}
		default:
			// Symlinks, devices and hard links are skipped: a link is the
			// other half of the traversal problem, and nothing in a server
			// directory needs one.
			continue
		}
	}
}

// resolveMember turns an archive member's name into a path inside the
// destination.
//
// The member name goes through the same validation as a request path, which
// refuses "..", absolute forms and the Windows hazards. Joining first and
// checking afterwards would not do: path.Join cleans as it joins, so
// "../../x" inside /unpacked quietly becomes /x — still inside the server,
// but not where the operator asked the archive to land, and files scattered
// by an extraction are exactly the Zip Slip outcome.
func (r *Root) resolveMember(destPrefix, name string) (string, error) {
	member, err := cleanRelative(name)
	if err != nil {
		return "", err
	}
	if member == "" {
		return "", fmt.Errorf("%w: the entry has no name", ErrInvalidPath)
	}
	return r.Resolve(path.Join(destPrefix, member))
}

// --- helpers ---

// writeAtomic writes through a temporary file in the same directory, so an
// interrupted write cannot leave a truncated file where a whole one was.
func writeAtomic(target string, src io.Reader, expected int64) error {
	_, err := writeAtomicCounted(target, io.LimitReader(src, expected))
	return err
}

func writeAtomicCounted(target string, src io.Reader) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return 0, fmt.Errorf("creating the parent of %s: %w", target, err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(target), ".write-*")
	if err != nil {
		return 0, fmt.Errorf("creating a temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath) // a no-op once renamed
	}()

	written, err := io.Copy(tmp, src)
	if err != nil {
		return 0, fmt.Errorf("writing %s: %w", target, err)
	}
	if err := tmp.Close(); err != nil {
		return 0, fmt.Errorf("closing %s: %w", target, err)
	}
	if err := os.Chmod(tmpPath, 0o640); err != nil {
		return 0, fmt.Errorf("setting permissions on %s: %w", target, err)
	}
	if err := os.Rename(tmpPath, target); err != nil {
		return 0, fmt.Errorf("replacing %s: %w", target, err)
	}
	return written, nil
}

func copyOneFile(from, to string) error {
	// Both ends were resolved before this is reached.
	src, err := os.Open(from) // #nosec G304 -- confined by Root.Resolve
	if err != nil {
		return fmt.Errorf("opening %s: %w", from, err)
	}
	defer func() { _ = src.Close() }()

	if _, err := writeAtomicCounted(to, src); err != nil {
		return err
	}
	return nil
}

func copyTree(from, to string) error {
	return filepath.Walk(from, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(from, p)
		if err != nil {
			return err
		}
		target := filepath.Join(to, rel)

		switch {
		case info.IsDir():
			return os.MkdirAll(target, 0o750)
		case info.Mode()&os.ModeSymlink != 0:
			// Skipped rather than recreated: a copied link could point out of
			// the root even though the original was harmless where it was.
			return nil
		default:
			return copyOneFile(p, target)
		}
	})
}

// isUnder reports whether target is inside base.
func isUnder(target, base string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// isBinary guesses whether content is text.
//
// A null byte is the practical test: no text file a server config editor
// should open contains one, and every binary format in a server directory
// does within the first few kilobytes.
func isBinary(content []byte) bool {
	limit := len(content)
	if limit > 8000 {
		limit = 8000
	}
	for _, b := range content[:limit] {
		if b == 0 {
			return true
		}
	}
	return false
}
