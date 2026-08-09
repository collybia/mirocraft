package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/collybia/mirocraft/internal/gamefiles"
	"github.com/collybia/mirocraft/internal/store"
)

// A proxy has no server.properties. Writing one it never opens would leave the
// panel publishing a port nothing listens on.
func TestAProxyGetsNoServerProperties(t *testing.T) {
	dir := t.TempDir()
	p := &Provisioner{log: silent()}

	if err := p.applyManagedConfig(proxyCore{}, &store.Server{ID: "01TEST", Port: 25577}, dir); err != nil {
		t.Fatalf("applyManagedConfig: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, gamefiles.PropertiesName)); !os.IsNotExist(err) {
		t.Errorf("a properties file was written for a proxy: %v", err)
	}
}

func TestVelocityPortIsApplied(t *testing.T) {
	dir := t.TempDir()
	config := "# Velocity config\n" +
		"config-version = \"2.7\"\n" +
		"bind = \"0.0.0.0:25577\"\n" +
		"motd = \"<#09add3>A Velocity Server\"\n"
	if err := os.WriteFile(filepath.Join(dir, velocityConfig), []byte(config), 0o640); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	p := &Provisioner{log: silent()}
	if err := p.applyManagedConfig(proxyCore{}, &store.Server{ID: "01TEST", Port: 25590}, dir); err != nil {
		t.Fatalf("applyManagedConfig: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, velocityConfig))
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	body := string(raw)

	if !strings.Contains(body, `bind = "0.0.0.0:25590"`) {
		t.Errorf("the port was not applied:\n%s", body)
	}
	// Everything else is the operator's.
	if !strings.Contains(body, "A Velocity Server") {
		t.Errorf("an operator's setting was lost:\n%s", body)
	}
	if !strings.Contains(body, "# Velocity config") {
		t.Errorf("comments were lost:\n%s", body)
	}
}

// The interface it was bound to is kept: an operator who bound the proxy to
// one address meant it, and rewriting that to 0.0.0.0 would expose a proxy
// they had deliberately kept local.
func TestVelocityKeepsTheInterface(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, velocityConfig),
		[]byte("bind = \"127.0.0.1:25577\"\n"), 0o640); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	p := &Provisioner{log: silent()}
	if err := p.applyManagedConfig(proxyCore{}, &store.Server{ID: "01TEST", Port: 25590}, dir); err != nil {
		t.Fatalf("applyManagedConfig: %v", err)
	}

	raw, _ := os.ReadFile(filepath.Join(dir, velocityConfig))
	if !strings.Contains(string(raw), `bind = "127.0.0.1:25590"`) {
		t.Errorf("the interface was not kept:\n%s", raw)
	}
}

func TestBungeeCordPortIsApplied(t *testing.T) {
	dir := t.TempDir()
	config := "listeners:\n" +
		"- query_port: 25577\n" +
		"  motd: '&1Another Bungee server'\n" +
		"  host: 0.0.0.0:25577\n" +
		"groups: {}\n"
	if err := os.WriteFile(filepath.Join(dir, bungeeCordConfig), []byte(config), 0o640); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	p := &Provisioner{log: silent()}
	if err := p.applyManagedConfig(proxyCore{}, &store.Server{ID: "01TEST", Port: 25591}, dir); err != nil {
		t.Fatalf("applyManagedConfig: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, bungeeCordConfig))
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	body := string(raw)

	if !strings.Contains(body, "host: 0.0.0.0:25591") {
		t.Errorf("the port was not applied:\n%s", body)
	}
	if !strings.Contains(body, "Another Bungee server") {
		t.Errorf("an operator's setting was lost:\n%s", body)
	}
	// The indentation matters in YAML: a rewritten line that lost its two
	// spaces moves the key out of the listener and BungeeCord refuses the file.
	if !strings.Contains(body, "\n  host: ") {
		t.Errorf("the indentation was not kept:\n%s", body)
	}
}

// A proxy writes its own config on first start, so there is nothing to edit
// yet. That must not be an error: it is the normal first start.
func TestNoProxyConfigYetIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	p := &Provisioner{log: silent()}

	if err := p.applyManagedConfig(proxyCore{}, &store.Server{ID: "01TEST", Port: 25577}, dir); err != nil {
		t.Fatalf("applyManagedConfig: %v", err)
	}
}

// Run before every start, so a config that already agrees must be left alone.
func TestApplyingTheProxyPortTwiceChangesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, velocityConfig)
	if err := os.WriteFile(path, []byte("bind = \"0.0.0.0:25590\"\n"), 0o640); err != nil {
		t.Fatalf("seeding: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	p := &Provisioner{log: silent()}
	if err := p.applyManagedConfig(proxyCore{}, &store.Server{ID: "01TEST", Port: 25590}, dir); err != nil {
		t.Fatalf("applyManagedConfig: %v", err)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("the file was rewritten although the port already agreed")
	}
}
