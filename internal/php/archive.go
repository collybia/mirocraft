package php

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// maxEntrySize caps a single extracted file. A PHP build's largest member is
// the interpreter at a few tens of megabytes.
const maxEntrySize = 512 << 20

// maxEntries caps how many members are extracted.
const maxEntries = 50_000

// decodeJSON is the one place this package parses a response body.
func decodeJSON(r io.Reader, v any) error {
	return json.NewDecoder(io.LimitReader(r, 8<<20)).Decode(v)
}

// safeJoin resolves an archive member against the destination and refuses
// anything that would land outside it.
//
// The same Zip Slip defence as internal/java: an archive is remote input, and
// a member named ../../etc/cron.d/x would be written wherever it asks.
func safeJoin(dir, name string) (string, error) {
	if name == "" {
		return "", errors.New("archive entry has an empty name")
	}
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
	// The archive this package downloaded.
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
		default:
			// A PHP build carries a few internal symlinks; devices and fifos
			// have no business in it.
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

	// The executable bit is preserved — without it the interpreter unpacks as
	// a file the kernel refuses to run — but nothing wider than the owner and
	// group is granted.
	perm := mode.Perm() & 0o750
	if perm == 0 {
		perm = 0o640
	}

	// target is confined to the destination by safeJoin above.
	file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm) // #nosec G304 -- confined by safeJoin
	if err != nil {
		return fmt.Errorf("creating %s: %w", target, err)
	}

	// LimitReader rather than trusting the header: a lying size field is how a
	// decompression bomb gets past a size check.
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

// extract unpacks an archive, choosing the format by extension.
func extract(archive, dir string) error {
	switch {
	case strings.HasSuffix(archive, ".zip"):
		return extractZip(archive, dir)
	case strings.HasSuffix(archive, ".tar.gz"):
		return extractTarGz(archive, dir)
	default:
		return fmt.Errorf("php: %s is not an archive this can unpack", filepath.Base(archive))
	}
}
