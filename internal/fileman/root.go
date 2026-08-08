// Package fileman confines file operations to one directory.
//
// Every path in the file API comes from a client, and a server directory sits
// next to every other server's data and next to the panel's own database. The
// whole point of this package is that there is exactly one function which
// turns a client-supplied path into a real one, so the defence can be read,
// reasoned about and tested in one place instead of being re-implemented in
// each handler.
package fileman

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Errors. Callers map ErrEscapes onto the documented path_traversal_denied.
var (
	ErrEscapes     = errors.New("path escapes the server directory")
	ErrInvalidPath = errors.New("path is not valid")
	ErrNotFound    = errors.New("path does not exist")
)

// reservedWindowsNames cannot be used as a file name on Windows: opening one
// talks to a device instead of a file. They are refused everywhere, so a
// server directory does not become unusable simply by being moved between
// hosts.
var reservedWindowsNames = map[string]struct{}{
	"con": {}, "prn": {}, "aux": {}, "nul": {},
	"com1": {}, "com2": {}, "com3": {}, "com4": {}, "com5": {},
	"com6": {}, "com7": {}, "com8": {}, "com9": {},
	"lpt1": {}, "lpt2": {}, "lpt3": {}, "lpt4": {}, "lpt5": {},
	"lpt6": {}, "lpt7": {}, "lpt8": {}, "lpt9": {},
}

// Root is a directory that file operations are confined to.
type Root struct {
	// dir is the root, absolute and with symlinks already resolved, so that
	// comparisons against resolved targets are like for like.
	dir string
}

// NewRoot returns a Root for dir, which must already exist.
//
// The root is resolved once, here: if the server directory is itself reached
// through a symlink — which it is on any host where the data directory is a
// mount or a link — then every later comparison would otherwise see the
// resolved child against the unresolved root and reject everything.
func NewRoot(dir string) (*Root, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("%w: the root is empty", ErrInvalidPath)
	}

	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolving the root %s: %w", dir, err)
	}

	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("resolving the root %s: %w", dir, err)
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("reading the root %s: %w", dir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%w: the root %s is not a directory", ErrInvalidPath, dir)
	}

	return &Root{dir: resolved}, nil
}

// Dir returns the resolved root.
func (r *Root) Dir() string { return r.dir }

// Resolve turns a client-supplied path into an absolute one inside the root.
//
// It handles the three ways out of a sandbox: lexical traversal with "..",
// an absolute path, and a symlink inside the root whose target is outside.
// The third is why this cannot be a string operation — the answer depends on
// what is actually on disk.
//
// The path need not exist; a caller creating a file resolves the target and
// gets the check against the deepest ancestor that does exist.
func (r *Root) Resolve(rel string) (string, error) {
	clean, err := cleanRelative(rel)
	if err != nil {
		return "", err
	}

	target := filepath.Join(r.dir, filepath.FromSlash(clean))

	// Compare against what the path really points at. Checking the literal
	// string would pass a symlink named "world" that points at /etc.
	resolved, err := resolveExistingPrefix(target)
	if err != nil {
		return "", err
	}
	if !within(r.dir, resolved) {
		return "", fmt.Errorf("%w: %s", ErrEscapes, rel)
	}

	return target, nil
}

// ResolveExisting is Resolve for a path that must already exist.
func (r *Root) ResolveExisting(rel string) (string, error) {
	target, err := r.Resolve(rel)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(target); err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%w: %s", ErrNotFound, rel)
		}
		return "", fmt.Errorf("reading %s: %w", rel, err)
	}
	return target, nil
}

// Rel turns an absolute path inside the root back into the slash-separated
// form the API speaks.
func (r *Root) Rel(abs string) (string, error) {
	rel, err := filepath.Rel(r.dir, abs)
	if err != nil {
		return "", fmt.Errorf("%w: %s", ErrEscapes, abs)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %s", ErrEscapes, abs)
	}
	if rel == "." {
		return "/", nil
	}
	return "/" + filepath.ToSlash(rel), nil
}

// cleanRelative validates a client path and returns it as a clean, relative,
// slash-separated string.
func cleanRelative(rel string) (string, error) {
	if strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("%w: it contains a null byte", ErrInvalidPath)
	}

	// Backslashes are separators on Windows, so a path written with them has
	// to be normalised before the traversal check rather than after — or
	// "..\\.." would sail past a check that only looks for "/".
	unified := strings.ReplaceAll(rel, "\\", "/")

	// A UNC or drive-letter prefix is an absolute path in disguise. Both are
	// checked before anything is trimmed, since trimming a leading slash
	// turns "//host/share" into something that no longer looks absolute.
	if strings.HasPrefix(unified, "//") {
		return "", fmt.Errorf("%w: absolute paths are not allowed", ErrInvalidPath)
	}
	if len(unified) >= 2 && unified[1] == ':' {
		return "", fmt.Errorf("%w: absolute paths are not allowed", ErrInvalidPath)
	}

	// A single leading slash means "the root of this server", not the
	// filesystem root, so it is dropped rather than treated as absolute.
	trimmed := strings.TrimPrefix(unified, "/")
	if trimmed == "" {
		return "", nil
	}

	// Cleaned WITHOUT a leading slash on purpose. path.Clean("/"+p) resolves
	// ".." against that slash and silently absorbs it, so "../secrets" comes
	// out as "/secrets" and a check that runs afterwards sees nothing wrong.
	// Cleaning the relative form leaves any escape at the front, where it can
	// be caught.
	clean := path.Clean(trimmed)
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%w: %s", ErrEscapes, rel)
	}

	for _, part := range strings.Split(clean, "/") {
		if err := checkComponent(part); err != nil {
			return "", err
		}
	}

	return clean, nil
}

// checkComponent rejects names that are unusable or dangerous.
func checkComponent(part string) error {
	if part == "" || part == "." {
		return nil
	}

	// A colon is an alternate data stream on Windows: "notes.txt:hidden"
	// writes to a stream nothing else lists. Rare enough in a server
	// directory that refusing it costs nothing.
	if strings.ContainsRune(part, ':') {
		return fmt.Errorf("%w: %q contains a colon", ErrInvalidPath, part)
	}

	base := strings.ToLower(part)
	if dot := strings.IndexByte(base, '.'); dot > 0 {
		base = base[:dot]
	}
	if _, reserved := reservedWindowsNames[base]; reserved {
		return fmt.Errorf("%w: %q is a reserved device name", ErrInvalidPath, part)
	}

	// A trailing dot or space is silently stripped by Windows, so a file
	// created as "x " would be reachable as "x" and vice versa.
	if strings.HasSuffix(part, " ") || strings.HasSuffix(part, ".") {
		return fmt.Errorf("%w: %q ends with a space or a dot", ErrInvalidPath, part)
	}

	return nil
}

// resolveExistingPrefix follows symlinks as far as the path exists.
//
// EvalSymlinks fails outright on a path whose last components are missing,
// which is the normal case when creating a file, so the deepest existing
// ancestor is resolved instead and the missing tail appended back.
func resolveExistingPrefix(target string) (string, error) {
	current := target
	var missing []string

	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			// Re-attach whatever did not exist yet.
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("resolving %s: %w", target, err)
		}

		parent := filepath.Dir(current)
		if parent == current {
			// Walked to the filesystem root without finding anything.
			return target, nil
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

// within reports whether path is the root or inside it.
func within(root, target string) bool {
	if target == root {
		return true
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
