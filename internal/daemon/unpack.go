package daemon

import (
	"archive/zip"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/collybia/mirocraft/internal/core"
)

// unpackMarkerPrefix names the file recording which archive was unpacked.
const unpackMarkerPrefix = ".mirocraft-unpacked"

// maxUnpackedEntry caps a single file taken out of an archive. The Bedrock
// server's largest member is a few hundred megabytes of resource pack.
const maxUnpackedEntry = 2 << 30 // 2 GiB

// maxUnpackedEntries caps how many members are extracted.
const maxUnpackedEntries = 50_000

// unpackArchive extracts a downloaded archive into the server directory.
//
// Once per build, marked like the installer step: the archive holds a hundred
// megabytes of executable and packs, and unpacking it on every start would add
// seconds to each one for a result already on disk.
//
// The operator's own files survive. Mojang ships server.properties, permissions
// and allowlist inside the archive, and overwriting those on an upgrade would
// throw away the whitelist and every setting the operator had chosen — so an
// existing file of those names is left as it is.
func (p *Provisioner) unpackArchive(build *core.Build, archivePath, dir string) error {
	marker := filepath.Join(dir, unpackMarkerFor(build))
	if _, err := os.Stat(marker); err == nil {
		return nil
	}

	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return fmt.Errorf("opening the %s archive: %w", build.Core, err)
	}
	defer func() { _ = reader.Close() }()

	if len(reader.File) > maxUnpackedEntries {
		return fmt.Errorf("the %s archive holds more than %d entries", build.Core, maxUnpackedEntries)
	}

	for _, entry := range reader.File {
		if err := extractEntry(entry, dir); err != nil {
			return err
		}
	}

	if err := os.WriteFile(marker, []byte(build.Version), 0o640); err != nil {
		return fmt.Errorf("recording the %s unpack: %w", build.Core, err)
	}

	p.log.Info("unpacked the server archive",
		slog.String("core", build.Core), slog.String("version", build.Version))
	return nil
}

// preserved names the files an operator owns once the server has run.
//
// Extracting over them turns an upgrade into a reset: the whitelist, the
// operator list and every setting go back to Mojang's defaults, and the
// operator finds out when their players cannot join.
var preserved = map[string]bool{
	"server.properties": true,
	"permissions.json":  true,
	"allowlist.json":    true,
	"whitelist.json":    true,
}

// extractEntry writes one archive member.
func extractEntry(entry *zip.File, dir string) error {
	target, err := safeExtractPath(dir, entry.Name)
	if err != nil {
		return err
	}

	if entry.FileInfo().IsDir() {
		return os.MkdirAll(target, 0o750)
	}
	if preserved[strings.ToLower(filepath.Base(target))] {
		if _, err := os.Stat(target); err == nil {
			return nil
		}
	}
	if entry.UncompressedSize64 > maxUnpackedEntry {
		return fmt.Errorf("archive entry %q is %d bytes, over the limit",
			entry.Name, entry.UncompressedSize64)
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(target), err)
	}

	source, err := entry.Open()
	if err != nil {
		return fmt.Errorf("reading %s from the archive: %w", entry.Name, err)
	}
	defer func() { _ = source.Close() }()

	// The executable bit is kept where the archive set it — without it the
	// Bedrock server unpacks as a file the kernel refuses to run — but nothing
	// wider than the owner and group is granted.
	perm := entry.FileInfo().Mode().Perm() & 0o750
	if perm == 0 {
		perm = 0o640
	}

	// target is confined to dir by safeExtractPath above.
	file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm) // #nosec G304 -- confined by safeExtractPath
	if err != nil {
		return fmt.Errorf("creating %s: %w", target, err)
	}

	// LimitReader rather than trusting the header: a lying size field is how a
	// decompression bomb gets past a size check.
	written, err := io.Copy(file, io.LimitReader(source, maxUnpackedEntry+1))
	closeErr := file.Close()

	if err != nil {
		return fmt.Errorf("writing %s: %w", target, err)
	}
	if closeErr != nil {
		return fmt.Errorf("closing %s: %w", target, closeErr)
	}
	if written > maxUnpackedEntry {
		_ = os.Remove(target)
		return fmt.Errorf("archive entry %q is larger than the limit", entry.Name)
	}
	return nil
}

// safeExtractPath resolves an archive member against the destination and
// refuses anything that would land outside it.
//
// The Zip Slip defence, the same one internal/java applies to runtimes: an
// archive is remote input, and a member named ../../etc/cron.d/x would
// otherwise be written wherever it asks.
func safeExtractPath(dir, name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("archive entry has an empty name")
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

// unpackMarkerFor names the marker for one build.
func unpackMarkerFor(build *core.Build) string {
	return unpackMarkerPrefix + "-" + build.Core + "-" + build.Version
}
