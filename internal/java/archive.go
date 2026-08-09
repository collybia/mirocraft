package java

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxEntrySize caps a single file extracted from an archive. A JRE's largest
// member is the modules file at a few hundred megabytes; the limit is there
// so a malicious or corrupt archive cannot fill the disk from one entry.
const maxEntrySize = 2 << 30 // 2 GiB

// maxEntries caps how many members are extracted, for the same reason.
const maxEntries = 100_000

// extract unpacks an archive into dir, choosing the format by extension.
func extract(archive, dir string) error {
	switch archiveSuffix(archive) {
	case ".tar.gz":
		return extractTarGz(archive, dir)
	case ".zip":
		return extractZip(archive, dir)
	default:
		return fmt.Errorf("%w: %s", ErrUnknownArchive, filepath.Base(archive))
	}
}

// safeJoin resolves an archive member's path against the destination and
// refuses anything that would land outside it.
//
// This is the Zip Slip defence. An archive is remote input, and a member
// named ../../etc/cron.d/x would otherwise be written wherever it asks, as
// the user the daemon runs as.
func safeJoin(dir, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("archive entry has an empty name")
	}
	// Archive paths always use forward slashes, whatever the host.
	clean := filepath.Clean(filepath.FromSlash(name))

	if filepath.IsAbs(clean) || strings.HasPrefix(clean, string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry %q is an absolute path", name)
	}
	for _, part := range strings.Split(filepath.ToSlash(clean), "/") {
		if part == ".." {
			return "", fmt.Errorf("archive entry %q escapes the destination", name)
		}
	}

	target := filepath.Join(dir, clean)

	// Belt and braces: even after the checks above, confirm the result is
	// still under the destination.
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return "", fmt.Errorf("resolving archive entry %q: %w", name, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry %q escapes the destination", name)
	}

	return target, nil
}

func extractTarGz(archive, dir string) error {
	// The archive this package downloaded from Adoptium.
	file, err := os.Open(archive) // #nosec G304 -- the runtime download
	if err != nil {
		return fmt.Errorf("opening %s: %w", archive, err)
	}
	defer func() { _ = file.Close() }()

	gz, err := gzip.NewReader(file)
	if err != nil {
		return fmt.Errorf("reading %s as gzip: %w", archive, err)
	}
	defer func() { _ = gz.Close() }()

	reader := tar.NewReader(gz)
	entries := 0

	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading %s: %w", archive, err)
		}

		entries++
		if entries > maxEntries {
			return fmt.Errorf("archive %s has more than %d entries", archive, maxEntries)
		}

		target, err := safeJoin(dir, header.Name)
		if err != nil {
			return err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o750); err != nil {
				return fmt.Errorf("creating %s: %w", target, err)
			}

		case tar.TypeReg:
			if header.Size > maxEntrySize {
				return fmt.Errorf("archive entry %q is %d bytes, over the limit",
					header.Name, header.Size)
			}
			if err := writeFile(target, reader, header.FileInfo().Mode(), header.Size); err != nil {
				return err
			}

		case tar.TypeSymlink:
			// Symlinks inside the archive are the other half of the traversal
			// problem: a link to /etc would let a later entry write through
			// it. A JRE contains a few internal links, so the target is
			// checked rather than the link refused outright.
			if err := writeSymlink(dir, target, header.Linkname); err != nil {
				return err
			}

		default:
			// Devices, fifos and hard links have no business in a JRE.
			continue
		}
	}
}

func extractZip(archive, dir string) error {
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("opening %s as zip: %w", archive, err)
	}
	defer func() { _ = reader.Close() }()

	if len(reader.File) > maxEntries {
		return fmt.Errorf("archive %s has more than %d entries", archive, maxEntries)
	}

	for _, entry := range reader.File {
		target, err := safeJoin(dir, entry.Name)
		if err != nil {
			return err
		}

		info := entry.FileInfo()
		if info.IsDir() {
			if err := os.MkdirAll(target, 0o750); err != nil {
				return fmt.Errorf("creating %s: %w", target, err)
			}
			continue
		}

		if entry.UncompressedSize64 > maxEntrySize {
			return fmt.Errorf("archive entry %q is %d bytes, over the limit",
				entry.Name, entry.UncompressedSize64)
		}

		body, err := entry.Open()
		if err != nil {
			return fmt.Errorf("reading %s from %s: %w", entry.Name, archive, err)
		}
		err = writeFile(target, body, info.Mode(), int64(entry.UncompressedSize64))
		_ = body.Close()
		if err != nil {
			return err
		}
	}

	return nil
}

// writeFile creates one extracted file, copying no more than declared.
func writeFile(target string, src io.Reader, mode os.FileMode, size int64) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(target), err)
	}

	// The executable bit is preserved — without it the java binary unpacks
	// as a file the kernel refuses to run — but nothing wider than the owner
	// and group is granted.
	perm := mode.Perm() & 0o750

	// target is checked against the destination by the caller before this.
	file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm) // #nosec G304 -- checked against the destination
	if err != nil {
		return fmt.Errorf("creating %s: %w", target, err)
	}

	// LimitReader rather than trusting the header: a lying size field is how
	// a decompression bomb gets past a size check.
	written, err := io.Copy(file, io.LimitReader(src, size+1))
	closeErr := file.Close()

	if err != nil {
		return fmt.Errorf("writing %s: %w", target, err)
	}
	if closeErr != nil {
		return fmt.Errorf("closing %s: %w", target, closeErr)
	}
	if written > size {
		_ = os.Remove(target)
		return fmt.Errorf("archive entry %q is larger than it declared", target)
	}

	return nil
}

// writeSymlink creates a link, refusing one that points outside the
// destination.
func writeSymlink(root, target, linkname string) error {
	resolved := linkname
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(filepath.Dir(target), filepath.FromSlash(linkname))
	}

	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("archive symlink %q points outside the destination", linkname)
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(target), err)
	}
	// A previous partial extraction may have left one behind.
	_ = os.Remove(target)

	if err := os.Symlink(filepath.FromSlash(linkname), target); err != nil {
		// Windows refuses symlinks without a privilege most services lack.
		// A JRE's links are internal conveniences, so skipping one is better
		// than failing the whole install.
		return nil //nolint:nilerr // deliberate: see comment
	}
	return nil
}
