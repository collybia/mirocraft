package daemon

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/collybia/mirocraft/internal/gamefiles"
	"github.com/collybia/mirocraft/internal/store"
)

func silent() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// The panel refuses to let anyone edit server-port through the settings API on
// the grounds that it owns it. This is the other half of that promise: without
// it the record said one port and the server listened on another, and two
// servers created on different ports both bound 25565 — the second failing to
// start with an address already in use, on a port neither the operator nor the
// panel had asked for.
func TestTheManagedPortReachesTheFile(t *testing.T) {
	dir := t.TempDir()
	p := &Provisioner{log: silent()}

	srv := &store.Server{ID: "01TEST", Port: 25566}
	if err := p.applyManagedProperties(srv, dir); err != nil {
		t.Fatalf("applyManagedProperties: %v", err)
	}

	properties, err := gamefiles.LoadProperties(dir)
	if err != nil {
		t.Fatalf("LoadProperties: %v", err)
	}
	if got, _ := properties.Get("server-port"); got != "25566" {
		t.Errorf("server-port = %q, want 25566", got)
	}
	// Empty binds every interface, which is what a panel-managed server needs.
	if got, ok := properties.Get("server-ip"); !ok || got != "" {
		t.Errorf("server-ip = %q (present=%v), want it present and empty", got, ok)
	}
}

// An operator's own settings must survive: the panel owns three keys, not the
// file.
func TestApplyingThePortKeepsEverythingElse(t *testing.T) {
	dir := t.TempDir()
	existing := "# a comment someone wrote\n" +
		"motd=my server\n" +
		"server-port=25565\n" +
		"some-modded-key=value\n"
	if err := os.WriteFile(filepath.Join(dir, gamefiles.PropertiesName), []byte(existing), 0o640); err != nil {
		t.Fatalf("seeding: %v", err)
	}

	p := &Provisioner{log: silent()}
	if err := p.applyManagedProperties(&store.Server{ID: "01TEST", Port: 25570}, dir); err != nil {
		t.Fatalf("applyManagedProperties: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, gamefiles.PropertiesName))
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	body := string(raw)

	if !strings.Contains(body, "server-port=25570") {
		t.Errorf("the port was not applied:\n%s", body)
	}
	if !strings.Contains(body, "motd=my server") {
		t.Errorf("an operator's setting was lost:\n%s", body)
	}
	if !strings.Contains(body, "some-modded-key=value") {
		t.Errorf("a key the panel does not know was dropped:\n%s", body)
	}
	if !strings.Contains(body, "# a comment someone wrote") {
		t.Errorf("comments were lost:\n%s", body)
	}
}

// Run before every start, so it must not rewrite a file that already agrees:
// a needless write on every start would change the file's timestamp and, on a
// running server, race the server's own writes.
func TestApplyingThePortTwiceChangesNothing(t *testing.T) {
	dir := t.TempDir()
	p := &Provisioner{log: silent()}
	srv := &store.Server{ID: "01TEST", Port: 25566}

	if err := p.applyManagedProperties(srv, dir); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	first, err := os.Stat(filepath.Join(dir, gamefiles.PropertiesName))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if err := p.applyManagedProperties(srv, dir); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	second, err := os.Stat(filepath.Join(dir, gamefiles.PropertiesName))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if !first.ModTime().Equal(second.ModTime()) {
		t.Error("the file was rewritten although nothing had changed")
	}
}

// A server without a port assigned yet is left alone rather than written with
// a zero, which would be a port no server can bind.
func TestNoPortMeansNoWrite(t *testing.T) {
	dir := t.TempDir()
	p := &Provisioner{log: silent()}

	if err := p.applyManagedProperties(&store.Server{ID: "01TEST"}, dir); err != nil {
		t.Fatalf("applyManagedProperties: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, gamefiles.PropertiesName)); !os.IsNotExist(err) {
		t.Errorf("a properties file was written for a server with no port: %v", err)
	}
}
