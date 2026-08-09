package modpack

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Every path in a pack is written by whoever published it, so this is the one
// thing that must not be got wrong: a pack naming ../../etc/cron.d/x would
// otherwise be installed exactly there, as the user the daemon runs as.
func TestPathsCannotEscape(t *testing.T) {
	dir := t.TempDir()

	for _, name := range []string{
		"../outside.jar",
		"mods/../../outside.jar",
		"/etc/cron.d/x",
		`..\outside.jar`,
		"",
		"   ",
	} {
		if _, err := SafePath(dir, name); err == nil {
			t.Errorf("SafePath accepted %q", name)
		}
	}

	// And the ordinary case still works.
	got, err := SafePath(dir, "mods/thing.jar")
	if err != nil {
		t.Fatalf("SafePath(mods/thing.jar): %v", err)
	}
	if !strings.HasPrefix(got, dir) {
		t.Errorf("resolved outside the directory: %s", got)
	}
}

// The index is a list of URLs written by a stranger. Without a host check it
// is a way to make the panel fetch anything from anywhere, including addresses
// only reachable from inside the operator's network.
func TestDownloadsMustComeFromAllowedHosts(t *testing.T) {
	for _, raw := range []string{
		"https://evil.example.com/mod.jar",
		"http://cdn.modrinth.com/mod.jar", // not https
		"https://169.254.169.254/latest/meta-data/",
		"file:///etc/passwd",
		"not a url at all",
	} {
		if err := checkHost(raw); err == nil {
			t.Errorf("checkHost accepted %q", raw)
		}
	}

	if err := checkHost("https://cdn.modrinth.com/data/x/versions/y/mod.jar"); err != nil {
		t.Errorf("checkHost refused Modrinth's own CDN: %v", err)
	}
}

// A modpack carries client-only mods. Installing them on a server is how a
// server fails to start with a stack trace about a missing client class.
func TestClientOnlyFilesAreSkipped(t *testing.T) {
	cases := []struct {
		env  map[string]string
		want bool
	}{
		{nil, true},
		{map[string]string{"client": "required", "server": "required"}, true},
		{map[string]string{"client": "required", "server": "optional"}, true},
		{map[string]string{"client": "required", "server": "unsupported"}, false},
	}

	for _, c := range cases {
		if got := (File{Env: c.env}).WantedOnServer(); got != c.want {
			t.Errorf("WantedOnServer(%v) = %v, want %v", c.env, got, c.want)
		}
	}
}

func TestLoaderIsReadFromTheDependencies(t *testing.T) {
	cases := []struct {
		deps map[string]string
		core string
	}{
		{map[string]string{"minecraft": "1.21.4", "fabric-loader": "0.16.9"}, "fabric"},
		{map[string]string{"minecraft": "1.20.1", "forge": "47.3.0"}, "forge"},
		{map[string]string{"minecraft": "1.21.1", "neoforge": "21.1.77"}, "neoforge"},
		{map[string]string{"minecraft": "1.20.1", "quilt-loader": "0.26.0"}, "quilt"},
	}

	for _, c := range cases {
		index := &Index{Dependencies: c.deps}
		loader, err := index.LoaderFor()
		if err != nil {
			t.Errorf("LoaderFor(%v): %v", c.deps, err)
			continue
		}
		if loader.Core != c.core {
			t.Errorf("LoaderFor(%v) = %q, want %q", c.deps, loader.Core, c.core)
		}
		if loader.Minecraft != c.deps["minecraft"] {
			t.Errorf("minecraft = %q, want %q", loader.Minecraft, c.deps["minecraft"])
		}
	}

	// A pack for a loader the panel does not install says so rather than
	// installing the wrong one.
	if _, err := (&Index{Dependencies: map[string]string{"minecraft": "1.21.4"}}).LoaderFor(); !errors.Is(err, ErrUnsupported) {
		t.Errorf("a pack with no loader: %v, want ErrUnsupported", err)
	}
	if _, err := (&Index{Dependencies: map[string]string{"fabric-loader": "0.16.9"}}).LoaderFor(); !errors.Is(err, ErrNotAPack) {
		t.Errorf("a pack with no minecraft version: %v, want ErrNotAPack", err)
	}
}

// The whole flow against a local server standing in for the CDN, including
// the checksum: a mod that arrives corrupted must not be installed, because
// what it produces is a server that fails to start for no visible reason.
func TestInstallFetchesAndVerifies(t *testing.T) {
	good := []byte("this is a mod jar, honestly")
	sum := sha512.Sum512(good)

	var (
		mu     sync.Mutex
		served string
	)
	// TLS, because the production code refuses anything but https and that
	// refusal is one of the things worth keeping.
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		served = r.URL.Path
		mu.Unlock()
		_, _ = w.Write(good)
	}))
	defer upstream.Close()

	// The host allowlist is the point of the production code, so the test
	// opens exactly one hole in it rather than turning it off.
	allowUpstream(t, upstream.URL)

	dir := t.TempDir()
	pack := buildPack(t, Index{
		FormatVersion: 1,
		Game:          "minecraft",
		Name:          "Test Pack",
		Dependencies:  map[string]string{"minecraft": "1.21.4", "fabric-loader": "0.16.9"},
		Files: []File{{
			Path:      "mods/good.jar",
			Hashes:    map[string]string{"sha512": hex.EncodeToString(sum[:])},
			Downloads: []string{upstream.URL + "/good.jar"},
			FileSize:  int64(len(good)),
		}},
	}, map[string]string{
		"overrides/config/thing.toml":        "from the pack",
		"server-overrides/config/thing.toml": "for servers only",
		"client-overrides/config/nope.toml":  "for clients only",
	})

	installer := &Installer{Client: upstream.Client()}
	index, err := installer.Install(context.Background(), pack, dir, nil)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if index.Name != "Test Pack" {
		t.Errorf("name = %q", index.Name)
	}
	mu.Lock()
	fetched := served
	mu.Unlock()
	if fetched != "/good.jar" {
		t.Errorf("fetched %q", fetched)
	}

	if _, err := os.Stat(filepath.Join(dir, "mods", "good.jar")); err != nil {
		t.Errorf("the mod was not installed: %v", err)
	}

	// server-overrides wins over overrides: that is what the format is for.
	body, err := os.ReadFile(filepath.Join(dir, "config", "thing.toml"))
	if err != nil {
		t.Fatalf("reading the override: %v", err)
	}
	if string(body) != "for servers only" {
		t.Errorf("override = %q, want the server one", body)
	}

	// Client overrides are for a launcher, not for a server.
	if _, err := os.Stat(filepath.Join(dir, "config", "nope.toml")); !os.IsNotExist(err) {
		t.Errorf("a client override was installed: %v", err)
	}
}

func TestACorruptedDownloadIsRefused(t *testing.T) {
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not what was promised"))
	}))
	defer upstream.Close()

	allowUpstream(t, upstream.URL)

	sum := sha512.Sum512([]byte("what was promised"))
	dir := t.TempDir()
	pack := buildPack(t, Index{
		FormatVersion: 1,
		Dependencies:  map[string]string{"minecraft": "1.21.4", "fabric-loader": "0.16.9"},
		Files: []File{{
			Path:      "mods/thing.jar",
			Hashes:    map[string]string{"sha512": hex.EncodeToString(sum[:])},
			Downloads: []string{upstream.URL + "/thing.jar"},
		}},
	}, nil)

	installer := &Installer{Client: upstream.Client()}
	_, err := installer.Install(context.Background(), pack, dir, nil)
	if !errors.Is(err, ErrChecksum) {
		t.Fatalf("Install: %v, want ErrChecksum", err)
	}
	// And nothing was left behind for the server to try to load.
	if _, err := os.Stat(filepath.Join(dir, "mods", "thing.jar")); !os.IsNotExist(err) {
		t.Errorf("the corrupted file was installed anyway: %v", err)
	}
}

// A pack whose index points at somewhere it may not is refused rather than
// fetched.
func TestAPackPointingElsewhereIsRefused(t *testing.T) {
	dir := t.TempDir()
	pack := buildPack(t, Index{
		FormatVersion: 1,
		Dependencies:  map[string]string{"minecraft": "1.21.4", "fabric-loader": "0.16.9"},
		Files: []File{{
			Path:      "mods/thing.jar",
			Downloads: []string{"https://evil.example.com/thing.jar"},
		}},
	}, nil)

	installer := &Installer{}
	if _, err := installer.Install(context.Background(), pack, dir, nil); !errors.Is(err, ErrDisallowed) {
		t.Fatalf("Install: %v, want ErrDisallowed", err)
	}
}

// An override naming a path outside the directory is refused before anything
// is written.
func TestAnEscapingOverrideIsRefused(t *testing.T) {
	dir := t.TempDir()
	pack := buildPack(t, Index{
		FormatVersion: 1,
		Dependencies:  map[string]string{"minecraft": "1.21.4", "fabric-loader": "0.16.9"},
	}, map[string]string{
		"overrides/../../escaped.txt": "should not be written",
	})

	installer := &Installer{}
	if _, err := installer.Install(context.Background(), pack, dir, nil); !errors.Is(err, ErrDisallowed) {
		t.Fatalf("Install: %v, want ErrDisallowed", err)
	}
}

func TestSomethingThatIsNotAPack(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.mrpack")

	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	if _, err := writer.Create("readme.txt"); err != nil {
		t.Fatalf("building: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o640); err != nil {
		t.Fatalf("writing: %v", err)
	}

	if _, err := ReadIndex(path); !errors.Is(err, ErrNotAPack) {
		t.Fatalf("ReadIndex: %v, want ErrNotAPack", err)
	}
}

// allowUpstream opens the host allowlist for a test server, and closes it
// again when the test ends. The allowlist is what stops a pack pointing the
// panel at somewhere it should not go, so a test turns it off for exactly one
// host rather than for everything.
func allowUpstream(t *testing.T, rawURL string) {
	t.Helper()

	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parsing the test server url: %v", err)
	}
	host := parsed.Hostname()
	allowedHosts[host] = true
	t.Cleanup(func() { delete(allowedHosts, host) })
}

// buildPack writes a .mrpack with the given index and extra entries.
func buildPack(t *testing.T, index Index, entries map[string]string) string {
	t.Helper()

	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)

	body, err := json.Marshal(index)
	if err != nil {
		t.Fatalf("encoding the index: %v", err)
	}
	file, err := writer.Create(IndexName)
	if err != nil {
		t.Fatalf("creating the index: %v", err)
	}
	if _, err := file.Write(body); err != nil {
		t.Fatalf("writing the index: %v", err)
	}

	for name, content := range entries {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatalf("creating %s: %v", name, err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("closing the archive: %v", err)
	}

	path := filepath.Join(t.TempDir(), "test.mrpack")
	if err := os.WriteFile(path, buf.Bytes(), 0o640); err != nil {
		t.Fatalf("writing the archive: %v", err)
	}
	return path
}
