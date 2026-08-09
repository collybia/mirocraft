package modpack

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// SafePath resolves a path from a pack against the server directory and
// refuses anything that would land outside it.
//
// Every path here is written by whoever published the pack, both the entry
// names in the archive and the "path" of each file in the index. A pack naming
// ../../etc/cron.d/x would otherwise be installed exactly there, as the user
// the daemon runs as.
func SafePath(dir, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("%w: a file with no path", ErrNotAPack)
	}

	clean := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q is an absolute path", ErrDisallowed, name)
	}
	for _, part := range strings.Split(filepath.ToSlash(clean), "/") {
		if part == ".." {
			return "", fmt.Errorf("%w: %q escapes the server directory", ErrDisallowed, name)
		}
	}

	target := filepath.Join(dir, clean)
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return "", fmt.Errorf("resolving %q: %w", name, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q escapes the server directory", ErrDisallowed, name)
	}
	return target, nil
}

// extractOverride writes one file from the archive into the server directory.
func extractOverride(entry *zip.File, dir, relative string) error {
	target, err := SafePath(dir, relative)
	if err != nil {
		return err
	}

	if entry.FileInfo().IsDir() {
		return os.MkdirAll(target, 0o750)
	}
	if entry.UncompressedSize64 > MaxFileBytes {
		return fmt.Errorf("%w: %s is %d bytes", ErrTooLarge, entry.Name, entry.UncompressedSize64)
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(target), err)
	}

	source, err := entry.Open()
	if err != nil {
		return fmt.Errorf("reading %s: %w", entry.Name, err)
	}
	defer func() { _ = source.Close() }()

	// target is confined to dir by SafePath above.
	file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640) // #nosec G304 -- confined by SafePath
	if err != nil {
		return fmt.Errorf("creating %s: %w", target, err)
	}

	// LimitReader rather than trusting the header: a lying size field is how a
	// decompression bomb gets past a size check.
	written, err := io.Copy(file, io.LimitReader(source, MaxFileBytes+1))
	closeErr := file.Close()

	if err != nil {
		return fmt.Errorf("writing %s: %w", target, err)
	}
	if closeErr != nil {
		return fmt.Errorf("closing %s: %w", target, closeErr)
	}
	if written > MaxFileBytes {
		_ = os.Remove(target)
		return fmt.Errorf("%w: %s is larger than it declared", ErrTooLarge, entry.Name)
	}
	return nil
}
