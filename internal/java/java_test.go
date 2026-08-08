package java

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- platform mapping ---

// Go and Adoptium spell the same targets differently, and getting it wrong
// yields a 404 that reads like the version does not exist.
func TestCurrentPlatformUsesAdoptiumNames(t *testing.T) {
	p, err := CurrentPlatform()
	if err != nil {
		t.Fatalf("CurrentPlatform on %s/%s: %v", runtime.GOOS, runtime.GOARCH, err)
	}

	switch runtime.GOARCH {
	case "amd64":
		if p.Arch != "x64" {
			t.Errorf("amd64 mapped to %q, want x64", p.Arch)
		}
	case "arm64":
		if p.Arch != "aarch64" {
			t.Errorf("arm64 mapped to %q, want aarch64", p.Arch)
		}
	}

	if runtime.GOOS == "darwin" && p.OS != "mac" {
		t.Errorf("darwin mapped to %q, want mac", p.OS)
	}
	if runtime.GOOS == "windows" && p.OS != "windows" {
		t.Errorf("windows mapped to %q", p.OS)
	}
}

func TestNormalizeRelease(t *testing.T) {
	tests := []struct{ in, want string }{
		{"jdk-21.0.12+8", "jdk-21.0.12_8"},
		{"jdk8u502-b07", "jdk8u502-b07"},
		{"jdk-25.0.4+7", "jdk-25.0.4_7"},
		{"a/b", "a_b"},
		{"..", ""},
		{"", ""},
	}

	for _, tc := range tests {
		if got := normalizeRelease(tc.in); got != tc.want {
			t.Errorf("normalizeRelease(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- archive extraction ---

// The Zip Slip family of bugs: an archive is remote input, and a member named
// ../../etc/cron.d/x would otherwise be written wherever it asks.
func TestSafeJoinRefusesEscapes(t *testing.T) {
	dir := t.TempDir()

	hostile := []string{
		"../escape",
		"../../etc/passwd",
		"a/../../escape",
		"/etc/passwd",
		"a/b/../../../escape",
		"",
	}

	for _, name := range hostile {
		if got, err := safeJoin(dir, name); err == nil {
			t.Errorf("safeJoin(%q) = %q, want an error", name, got)
		}
	}

	safe := []string{"a", "a/b", "./a/b", "jdk-21/bin/java"}
	for _, name := range safe {
		got, err := safeJoin(dir, name)
		if err != nil {
			t.Errorf("safeJoin(%q): %v", name, err)
			continue
		}
		if !strings.HasPrefix(got, dir) {
			t.Errorf("safeJoin(%q) = %q, which is outside %q", name, got, dir)
		}
	}
}

// makeTarGz builds a tar.gz from a map of path to content.
func makeTarGz(t *testing.T, dir string, files map[string]string) string {
	t.Helper()

	path := filepath.Join(dir, "runtime.tar.gz")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating the archive: %v", err)
	}
	defer func() { _ = file.Close() }()

	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)

	for name, content := range files {
		mode := int64(0o644)
		if strings.HasSuffix(name, "/java") || strings.HasSuffix(name, "/java.exe") {
			mode = 0o755
		}
		header := &tar.Header{
			Name: name, Mode: mode, Size: int64(len(content)), Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(header); err != nil {
			t.Fatalf("writing header: %v", err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("writing content: %v", err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing gzip: %v", err)
	}
	return path
}

func TestExtractTarGz(t *testing.T) {
	dir := t.TempDir()
	archive := makeTarGz(t, dir, map[string]string{
		"jdk-21/bin/java":   "#!/bin/sh\necho java\n",
		"jdk-21/lib/rt.jar": "jar bytes",
		"jdk-21/release":    "JAVA_VERSION=21\n",
	})

	dest := filepath.Join(dir, "out")
	if err := extract(archive, dest); err != nil {
		t.Fatalf("extract: %v", err)
	}

	for _, name := range []string{"jdk-21/bin/java", "jdk-21/lib/rt.jar", "jdk-21/release"} {
		if _, err := os.Stat(filepath.Join(dest, filepath.FromSlash(name))); err != nil {
			t.Errorf("%s was not extracted: %v", name, err)
		}
	}

	// The executable bit has to survive, or the unpacked java is a file the
	// kernel refuses to run.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dest, "jdk-21", "bin", "java"))
		if err != nil {
			t.Fatalf("stat: %v", err)
		}
		if info.Mode().Perm()&0o100 == 0 {
			t.Errorf("java was extracted without the executable bit: %v", info.Mode())
		}
	}
}

// A tar entry that escapes the destination must abort the extraction.
func TestExtractTarGzRefusesTraversal(t *testing.T) {
	dir := t.TempDir()
	archive := makeTarGz(t, dir, map[string]string{
		"../escaped.txt": "you should not see this",
	})

	dest := filepath.Join(dir, "out")
	if err := extract(archive, dest); err == nil {
		t.Fatal("extract accepted an entry that escapes the destination")
	}

	if _, err := os.Stat(filepath.Join(dir, "escaped.txt")); !os.IsNotExist(err) {
		t.Fatal("the traversing entry was written outside the destination")
	}
}

func makeZip(t *testing.T, dir string, files map[string]string) string {
	t.Helper()

	path := filepath.Join(dir, "runtime.zip")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating the archive: %v", err)
	}
	defer func() { _ = file.Close() }()

	zw := zip.NewWriter(file)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("creating entry: %v", err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatalf("writing entry: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing zip: %v", err)
	}
	return path
}

func TestExtractZip(t *testing.T) {
	dir := t.TempDir()
	archive := makeZip(t, dir, map[string]string{
		"jdk-21/bin/java.exe": "MZ fake executable",
		"jdk-21/release":      "JAVA_VERSION=21\n",
	})

	dest := filepath.Join(dir, "out")
	if err := extract(archive, dest); err != nil {
		t.Fatalf("extract: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dest, "jdk-21", "bin", "java.exe")); err != nil {
		t.Fatalf("java.exe was not extracted: %v", err)
	}
}

func TestExtractZipRefusesTraversal(t *testing.T) {
	dir := t.TempDir()
	archive := makeZip(t, dir, map[string]string{
		"../escaped.txt": "you should not see this",
	})

	dest := filepath.Join(dir, "out")
	if err := extract(archive, dest); err == nil {
		t.Fatal("extract accepted an entry that escapes the destination")
	}
	if _, err := os.Stat(filepath.Join(dir, "escaped.txt")); !os.IsNotExist(err) {
		t.Fatal("the traversing entry was written outside the destination")
	}
}

func TestExtractRejectsUnknownFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.rar")
	if err := os.WriteFile(path, []byte("not an archive"), 0o600); err != nil {
		t.Fatalf("writing: %v", err)
	}

	if err := extract(path, filepath.Join(dir, "out")); !errors.Is(err, ErrUnknownArchive) {
		t.Fatalf("extract = %v, want ErrUnknownArchive", err)
	}
}

// A member that claims one size and delivers more is how a decompression
// bomb gets past a size check.
func TestExtractRefusesAnEntryLargerThanDeclared(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lying.tar.gz")

	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("creating: %v", err)
	}
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)

	// Declare 4 bytes, then write far more directly into the stream.
	if err := tw.WriteHeader(&tar.Header{
		Name: "big.bin", Mode: 0o644, Size: 4, Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("header: %v", err)
	}
	_, _ = tw.Write([]byte("0123"))
	_ = tw.Flush()
	// Bypass the tar writer's own accounting.
	_, _ = gz.Write(bytes.Repeat([]byte("x"), 1024))
	_ = tw.Close()
	_ = gz.Close()
	_ = file.Close()

	// Either the tar reader rejects the stream or writeFile catches the
	// overrun; both are acceptable, silently writing 1 KB is not.
	dest := filepath.Join(dir, "out")
	if err := extract(path, dest); err == nil {
		if info, statErr := os.Stat(filepath.Join(dest, "big.bin")); statErr == nil && info.Size() > 4 {
			t.Fatalf("wrote %d bytes for an entry that declared 4", info.Size())
		}
	}
}

func TestArchiveSuffix(t *testing.T) {
	tests := []struct{ in, want string }{
		{"OpenJDK21U-jre_x64_linux_hotspot_21.0.12_8.tar.gz", ".tar.gz"},
		{"OpenJDK21U-jre_x64_windows_hotspot_21.0.12_8.zip", ".zip"},
		{"thing.TAR.GZ", ".tar.gz"},
		{"thing.rar", ".rar"},
	}

	for _, tc := range tests {
		if got := archiveSuffix(tc.in); got != tc.want {
			t.Errorf("archiveSuffix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// --- finding the binary ---

func TestFindJavaBinary(t *testing.T) {
	dir := t.TempDir()

	// Temurin archives nest the runtime under a release-named directory.
	binDir := filepath.Join(dir, "jdk-21.0.12+8", "bin")
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	target := filepath.Join(binDir, binaryName())
	if err := os.WriteFile(target, []byte("binary"), 0o750); err != nil {
		t.Fatalf("writing: %v", err)
	}

	got, err := findJavaBinary(dir)
	if err != nil {
		t.Fatalf("findJavaBinary: %v", err)
	}
	if got != target {
		t.Fatalf("found %q, want %q", got, target)
	}
}

// A copy of the name outside bin/ must not be mistaken for the real thing.
func TestFindJavaBinaryIgnoresStrayCopies(t *testing.T) {
	dir := t.TempDir()

	stray := filepath.Join(dir, "jdk-21", "legal", binaryName())
	if err := os.MkdirAll(filepath.Dir(stray), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(stray, []byte("not the binary"), 0o640); err != nil {
		t.Fatalf("writing: %v", err)
	}

	if _, err := findJavaBinary(dir); !errors.Is(err, ErrNotExecutable) {
		t.Fatalf("findJavaBinary = %v, want ErrNotExecutable", err)
	}
}

func TestFindJavaBinaryMissing(t *testing.T) {
	if _, err := findJavaBinary(t.TempDir()); !errors.Is(err, ErrNotExecutable) {
		t.Fatalf("findJavaBinary on an empty tree = %v, want ErrNotExecutable", err)
	}
}

// --- the manager ---

// adoptiumStub serves an Adoptium-shaped response and a runtime archive whose
// contents are a script that behaves like `java -version`.
func adoptiumStub(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()

	archive := buildFakeRuntime(t)
	sum := sha256.Sum256(archive)

	var apiHits int32
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	mux.HandleFunc("/assets/latest/", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&apiHits, 1)

		// The path carries the major: /assets/latest/21/hotspot
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		major := parts[len(parts)-2]

		if major == "99" { // a major Adoptium does not build
			w.WriteHeader(http.StatusNotFound)
			return
		}

		fmt.Fprintf(w, `[{
			"release_name": "jdk-%s.0.1+9",
			"binary": {
				"os": "linux", "architecture": "x64", "image_type": "jre",
				"package": {
					"name": "OpenJDK%sU-jre_x64_linux_hotspot.tar.gz",
					"link": %q, "size": %d, "checksum": %q
				}
			}
		}]`, major, major, server.URL+"/download/runtime.tar.gz",
			len(archive), hex.EncodeToString(sum[:]))
	})

	mux.HandleFunc("/download/runtime.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	})

	return server, &apiHits
}

// buildFakeRuntime produces a tar.gz holding an executable that answers
// -version, so the manager's verification step is exercised for real.
func buildFakeRuntime(t *testing.T) []byte {
	t.Helper()

	var script string
	name := "bin/" + binaryName()
	if runtime.GOOS == "windows" {
		// A batch file cannot be exec'd directly, so on Windows the test
		// copies the real java-like behaviour with a tiny .exe stand-in: the
		// Go test binary itself, which exits 0 for any argument.
		script = ""
	} else {
		script = "#!/bin/sh\necho 'openjdk version \"21.0.1\"' >&2\nexit 0\n"
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)

	content := []byte(script)
	if runtime.GOOS == "windows" {
		self, err := os.ReadFile(os.Args[0])
		if err != nil {
			t.Fatalf("reading the test binary: %v", err)
		}
		content = self
	}

	if err := tw.WriteHeader(&tar.Header{
		Name: "jdk-21.0.1+9/" + name, Mode: 0o755,
		Size: int64(len(content)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("header: %v", err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("closing tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("closing gzip: %v", err)
	}
	return buf.Bytes()
}

func newTestManager(t *testing.T, server *httptest.Server) *Manager {
	t.Helper()

	m := NewManager(t.TempDir(), discardLogger())
	m.APIBase = server.URL
	m.Client = server.Client()
	m.Platform = &Platform{OS: "linux", Arch: "x64"}
	return m
}

func TestEnsureInstallsARuntime(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake runtime is a shell script, which Windows cannot exec")
	}

	server, _ := adoptiumStub(t)
	m := newTestManager(t, server)

	rt, err := m.Ensure(context.Background(), 21)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if rt.Major != 21 {
		t.Errorf("major = %d, want 21", rt.Major)
	}
	if rt.Bin == "" {
		t.Fatal("no binary path")
	}
	if _, err := os.Stat(rt.Bin); err != nil {
		t.Fatalf("the binary is not on disk: %v", err)
	}
	if !strings.Contains(rt.Dir, filepath.Join(m.Dir, "21")) {
		t.Errorf("installed into %q, want a subdirectory of the 21 directory", rt.Dir)
	}
}

// The second call must not touch the network.
func TestEnsureIsCached(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake runtime is a shell script, which Windows cannot exec")
	}

	server, hits := adoptiumStub(t)
	m := newTestManager(t, server)

	for i := 0; i < 3; i++ {
		if _, err := m.Ensure(context.Background(), 21); err != nil {
			t.Fatalf("Ensure %d: %v", i, err)
		}
	}

	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("the API was called %d times, want 1", got)
	}
}

// Two servers starting at once must not download the same runtime twice or
// unpack over each other.
func TestEnsureIsSerializedPerMajor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake runtime is a shell script, which Windows cannot exec")
	}

	server, hits := adoptiumStub(t)
	m := newTestManager(t, server)

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = m.Ensure(context.Background(), 21)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent Ensure %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("the API was called %d times for one major, want 1", got)
	}
}

// A runtime installed by an earlier run must be found on disk rather than
// downloaded again.
func TestScanFindsInstalledRuntimes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake runtime is a shell script, which Windows cannot exec")
	}

	server, hits := adoptiumStub(t)
	m := newTestManager(t, server)

	if _, err := m.Ensure(context.Background(), 21); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// A fresh manager over the same directory, as after a restart.
	restarted := NewManager(m.Dir, discardLogger())
	restarted.APIBase = server.URL
	restarted.Client = server.Client()
	restarted.Platform = m.Platform
	restarted.Scan(context.Background())

	if len(restarted.Installed()) != 1 {
		t.Fatalf("Scan found %d runtimes, want 1", len(restarted.Installed()))
	}

	before := atomic.LoadInt32(hits)
	if _, err := restarted.Ensure(context.Background(), 21); err != nil {
		t.Fatalf("Ensure after Scan: %v", err)
	}
	if atomic.LoadInt32(hits) != before {
		t.Fatal("Ensure re-downloaded a runtime that was already on disk")
	}
}

func TestEnsureUnsupportedMajor(t *testing.T) {
	server, _ := adoptiumStub(t)
	m := newTestManager(t, server)

	if _, err := m.Ensure(context.Background(), 99); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Ensure(99) = %v, want ErrUnsupported", err)
	}
}

// A runtime whose bytes do not match the published checksum must not be
// installed.
func TestInstallRejectsABadChecksum(t *testing.T) {
	archive := buildFakeRuntime(t)

	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	mux.HandleFunc("/assets/latest/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `[{
			"release_name": "jdk-21.0.1+9",
			"binary": {"os":"linux","architecture":"x64","image_type":"jre",
				"package": {"name":"r.tar.gz","link":%q,"size":%d,"checksum":%q}}
		}]`, server.URL+"/r.tar.gz", len(archive), strings.Repeat("0", 64))
	})
	mux.HandleFunc("/r.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(archive)
	})

	m := newTestManager(t, server)

	if _, err := m.Ensure(context.Background(), 21); err == nil {
		t.Fatal("Ensure accepted a runtime that failed its checksum")
	}
	if len(m.Installed()) != 0 {
		t.Fatal("the rejected runtime was recorded as installed")
	}
}

// An unpacked tree that does not execute is worse than none: it would make
// every server start fail with a confusing error.
func TestDiscoverRemovesAnUnusableRuntime(t *testing.T) {
	m := NewManager(t.TempDir(), discardLogger())

	dir := filepath.Join(m.Dir, "21", "jdk-broken")
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Present, named right, and not a working program.
	if err := os.WriteFile(filepath.Join(binDir, binaryName()), []byte("garbage"), 0o750); err != nil {
		t.Fatalf("writing: %v", err)
	}

	if _, err := m.discover(context.Background(), 21); !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("discover = %v, want ErrNotInstalled", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("the unusable runtime was left in place")
	}
}

func TestRuntimeString(t *testing.T) {
	rt := &Runtime{Major: 21, Release: "jdk-21.0.12+8"}
	if got := rt.String(); !strings.Contains(got, "21") || !strings.Contains(got, "jdk-21.0.12+8") {
		t.Fatalf("String() = %q", got)
	}

	bare := &Runtime{Major: 8}
	if got := bare.String(); got != "Java 8" {
		t.Fatalf("String() = %q, want \"Java 8\"", got)
	}
}

// The per-major directory does not exist on a cold start, and staging the
// unpacked runtime inside it failed for exactly that reason the first time
// this ran against a real Adoptium download.
func TestEnsureWorksWhenNothingHasBeenInstalledYet(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake runtime is a shell script, which Windows cannot exec")
	}

	server, _ := adoptiumStub(t)
	m := newTestManager(t, server)

	// Nothing has created the runtime root, let alone the per-major directory.
	if err := os.RemoveAll(m.Dir); err != nil {
		t.Fatalf("clearing the runtime dir: %v", err)
	}

	rt, err := m.Ensure(context.Background(), 21)
	if err != nil {
		t.Fatalf("Ensure on a completely empty data directory: %v", err)
	}
	if _, err := os.Stat(rt.Bin); err != nil {
		t.Fatalf("the binary is not on disk: %v", err)
	}
}
