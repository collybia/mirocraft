package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/collybia/mirocraft/internal/store"
)

func backends() []*store.Server {
	return []*store.Server{
		{ID: "01B", Name: "creative", Port: 25567},
		{ID: "01A", Name: "survival", Port: 25566},
	}
}

// The panel allocated every backend's port, so keeping this list by hand would
// be asking an operator to copy numbers the panel already has.
func TestVelocityBackendsAreWritten(t *testing.T) {
	dir := t.TempDir()
	config := "bind = \"0.0.0.0:25577\"\n\n" +
		"[servers]\n" +
		"lobby = \"127.0.0.1:30066\"\n" +
		"try = [\"lobby\"]\n\n" +
		"[forced-hosts]\n" +
		"\"lobby.example.com\" = [\"lobby\"]\n"
	if err := os.WriteFile(filepath.Join(dir, velocityConfig), []byte(config), 0o640); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	p := &Provisioner{log: silent()}
	if err := p.applyProxyBackends(&store.Server{ID: "01PROXY"}, dir, backends()); err != nil {
		t.Fatalf("applyProxyBackends: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, velocityConfig))
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	body := string(raw)

	if !strings.Contains(body, `creative = "127.0.0.1:25567"`) ||
		!strings.Contains(body, `survival = "127.0.0.1:25566"`) {
		t.Errorf("the backends were not written:\n%s", body)
	}
	// The old entry is gone: the table is the panel's, and merging entry by
	// entry would leave an unlinked server still listed.
	if strings.Contains(body, "lobby = ") {
		t.Errorf("a stale backend survived:\n%s", body)
	}
	// A proxy with no try list drops everyone who connects.
	if !strings.Contains(body, `try = ["creative"]`) {
		t.Errorf("no try list was written:\n%s", body)
	}
	// Everything after the table is the operator's.
	if !strings.Contains(body, "[forced-hosts]") {
		t.Errorf("the section after the table was eaten:\n%s", body)
	}
	if !strings.Contains(body, `bind = "0.0.0.0:25577"`) {
		t.Errorf("the section before the table was eaten:\n%s", body)
	}
}

func TestBungeeBackendsAreWritten(t *testing.T) {
	dir := t.TempDir()
	config := "listeners:\n" +
		"- host: 0.0.0.0:25577\n" +
		"servers:\n" +
		"  lobby:\n" +
		"    address: localhost:30066\n" +
		"    motd: 'old'\n" +
		"timeout: 30000\n"
	if err := os.WriteFile(filepath.Join(dir, bungeeCordConfig), []byte(config), 0o640); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	p := &Provisioner{log: silent()}
	if err := p.applyProxyBackends(&store.Server{ID: "01PROXY"}, dir, backends()); err != nil {
		t.Fatalf("applyProxyBackends: %v", err)
	}

	raw, _ := os.ReadFile(filepath.Join(dir, bungeeCordConfig))
	body := string(raw)

	if !strings.Contains(body, "address: 127.0.0.1:25566") ||
		!strings.Contains(body, "address: 127.0.0.1:25567") {
		t.Errorf("the backends were not written:\n%s", body)
	}
	if strings.Contains(body, "30066") {
		t.Errorf("a stale backend survived:\n%s", body)
	}
	// The keys after the block must survive: a replacement that swallowed
	// them would leave BungeeCord with no timeout and no listeners.
	if !strings.Contains(body, "timeout: 30000") {
		t.Errorf("a later key was eaten:\n%s", body)
	}
	if !strings.Contains(body, "listeners:") {
		t.Errorf("an earlier key was eaten:\n%s", body)
	}
}

// Nothing linked is not the same as "empty the list": an operator may have
// written entries by hand, and wiping them because the panel has nothing to
// say would be the panel claiming a file it has never written to.
func TestNoBackendsLeavesTheConfigAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, velocityConfig)
	config := "[servers]\nlobby = \"127.0.0.1:30066\"\n"
	if err := os.WriteFile(path, []byte(config), 0o640); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	p := &Provisioner{log: silent()}
	if err := p.applyProxyBackends(&store.Server{ID: "01PROXY"}, dir, nil); err != nil {
		t.Fatalf("applyProxyBackends: %v", err)
	}

	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "lobby") {
		t.Errorf("the operator's own entry was removed:\n%s", raw)
	}
}

// A server name is not a TOML key: spaces and anything else have to go, or
// Velocity refuses the whole file.
func TestVelocityKeysAreSafe(t *testing.T) {
	cases := map[string]string{
		"survival":       "survival",
		"My Server":      "my-server",
		"выживание":      "server",
		"a.b":            "a-b",
		"--":             "server",
		"Creative_World": "creative_world",
	}

	for given, want := range cases {
		if got := velocityKey(given); got != want {
			t.Errorf("velocityKey(%q) = %q, want %q", given, got, want)
		}
	}
}

// The secret is what carries a player's identity from the proxy to the
// backend. One the proxy already wrote must never be replaced: the backends
// know it, and a new one locks everybody out.
func TestForwardingSecretIsKeptOnceWritten(t *testing.T) {
	dir := t.TempDir()
	p := &Provisioner{log: silent()}

	first, err := p.ensureForwardingSecret(dir)
	if err != nil {
		t.Fatalf("ensureForwardingSecret: %v", err)
	}
	if len(first) < 16 {
		t.Errorf("secret %q is too short to be one", first)
	}

	second, err := p.ensureForwardingSecret(dir)
	if err != nil {
		t.Fatalf("second ensureForwardingSecret: %v", err)
	}
	if second != first {
		t.Error("the secret was replaced; every backend would stop being trusted")
	}
}
