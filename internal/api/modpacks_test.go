package api

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/collybia/mirocraft/internal/catalog"
	"github.com/collybia/mirocraft/internal/core"
)

// packCatalog serves one modpack version, so these tests are about the
// endpoint rather than about Modrinth being up.
type packCatalog struct {
	stubCatalog
	versions []catalog.Version
}

func (p *packCatalog) Versions(context.Context, string, string, string) ([]catalog.Version, error) {
	return p.versions, nil
}

// packServer serves a .mrpack built on the spot.
//
// The index names no downloads: the modpack package refuses a download from
// anywhere but the registries' own hosts, and that refusal is worth keeping in
// place here — it is exercised properly in that package's own tests.
func packServer(t *testing.T, index map[string]any, entries map[string]string) (*httptest.Server, []byte) {
	t.Helper()

	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)

	body, err := json.Marshal(index)
	if err != nil {
		t.Fatalf("encoding the index: %v", err)
	}
	file, err := writer.Create("modrinth.index.json")
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

	pack := buf.Bytes()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(pack)
	}))
	t.Cleanup(server.Close)
	return server, pack
}

// fabricPack wires a catalogue serving one Fabric pack for Minecraft 1.21.1.
func fabricPack(t *testing.T, env *testEnv, entries map[string]string) *packCatalog {
	t.Helper()

	upstream, pack := packServer(t, map[string]any{
		"formatVersion": 1,
		"game":          "minecraft",
		"name":          "Test Pack",
		"versionId":     "1.2.3",
		"dependencies": map[string]string{
			"minecraft":     "1.21.1",
			"fabric-loader": "0.16.9",
		},
	}, entries)

	stub := &packCatalog{versions: []catalog.Version{{
		ID: "pv1", ProjectID: "pack-abc", Name: "Test Pack 1.2.3", Number: "1.2.3",
		Channel: "release", Loaders: []string{"fabric"}, GameVersions: []string{"1.21.1"},
		Files: []catalog.File{{
			Name: "test-pack-1.2.3.mrpack", URL: upstream.URL,
			Size: int64(len(pack)), Primary: true, SHA512: sha512Of(pack),
		}},
	}}}

	env.api.catalog = stub
	env.api.cores = core.DefaultRegistry(nil)
	return stub
}

// modpackToken carries both scopes the endpoint needs.
func (e *testEnv) modpackToken() string {
	e.t.Helper()
	return e.mintToken(e.user.ID, []string{ScopeServersRead, ScopeServersWrite, ScopeFilesWrite})
}

// The whole point of the feature: one call turns a Paper server into the
// server the pack describes, loader and Minecraft version included.
func TestInstallingAModpackSwitchesTheServerToItsLoader(t *testing.T) {
	env := newTestEnv(t)
	fabricPack(t, env, map[string]string{
		"overrides/config/pack.toml":        "from the pack",
		"server-overrides/config/pack.toml": "for servers only",
		"client-overrides/config/nope.toml": "for clients only",
	})

	dir := env.api.serverDir(env.serverRecord)
	// A mod left from whatever was installed before. It must not survive: the
	// pack's list of mods is the whole list, and a stray jar is a duplicate
	// mod error on start that nothing traces back to here.
	if err := os.MkdirAll(filepath.Join(dir, "mods"), 0o750); err != nil {
		t.Fatalf("seeding mods/: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mods", "stale.jar"), []byte("old"), 0o600); err != nil {
		t.Fatalf("seeding a stale mod: %v", err)
	}

	resp := env.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/modpack",
		map[string]any{"project_id": "pack-abc"}, env.modpackToken())
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	body := decodeJSON[struct {
		TaskID string       `json:"task_id"`
		Plan   *modpackPlan `json:"plan"`
	}](t, resp)

	if body.Plan.Core != "fabric" || body.Plan.Minecraft != "1.21.1" || !body.Plan.ChangesCore {
		t.Fatalf("plan = %+v", body.Plan)
	}
	env.waitForTask(t, body.TaskID, TaskDone, 30*time.Second)

	server, err := env.db.Servers.GetByID(context.Background(), testServerID)
	if err != nil {
		t.Fatalf("reading the server back: %v", err)
	}
	if server.Core != "fabric" || server.Version != "1.21.1" {
		t.Errorf("the server is still %s %s", server.Core, server.Version)
	}
	// The jar about to be installed is a different one, so the remembered
	// name must not survive the switch.
	if server.JarName != "" {
		t.Errorf("jar name = %q, want it cleared", server.JarName)
	}

	if _, err := os.Stat(filepath.Join(dir, "mods", "stale.jar")); !os.IsNotExist(err) {
		t.Errorf("the stale mod survived the install: %v", err)
	}

	// server-overrides wins over overrides, and client overrides are for a
	// launcher rather than for a server.
	config, err := os.ReadFile(filepath.Join(dir, "config", "pack.toml"))
	if err != nil {
		t.Fatalf("reading the override: %v", err)
	}
	if string(config) != "for servers only" {
		t.Errorf("override = %q, want the server one", config)
	}
	if _, err := os.Stat(filepath.Join(dir, "config", "nope.toml")); !os.IsNotExist(err) {
		t.Error("a client override was installed")
	}

	installed := decodeJSON[struct {
		Installed *installedModpack `json:"installed"`
	}](t, env.do(http.MethodGet, "/api/v1/servers/"+testServerID+"/modpack", nil, env.modpackToken()))
	if installed.Installed == nil {
		t.Fatal("the server reports no modpack after installing one")
	}
	if installed.Installed.Name != "Test Pack" || installed.Installed.Core != "fabric" {
		t.Errorf("record = %+v", installed.Installed)
	}
}

// A dry run is what the panel shows before asking "are you sure": it must
// change nothing at all.
func TestAModpackDryRunChangesNothing(t *testing.T) {
	env := newTestEnv(t)
	fabricPack(t, env, nil)

	dir := env.api.serverDir(env.serverRecord)
	if err := os.MkdirAll(filepath.Join(dir, "mods"), 0o750); err != nil {
		t.Fatalf("seeding mods/: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mods", "stale.jar"), []byte("old"), 0o600); err != nil {
		t.Fatalf("seeding a stale mod: %v", err)
	}

	plan := decodeJSON[modpackPlan](t, env.do(http.MethodPost,
		"/api/v1/servers/"+testServerID+"/modpack",
		map[string]any{"project_id": "pack-abc", "dry_run": true}, env.modpackToken()))

	if plan.Core != "fabric" || plan.ReplacesDir != "mods" {
		t.Fatalf("plan = %+v", plan)
	}

	if _, err := os.Stat(filepath.Join(dir, "mods", "stale.jar")); err != nil {
		t.Errorf("a dry run deleted a mod: %v", err)
	}
	server, err := env.db.Servers.GetByID(context.Background(), testServerID)
	if err != nil {
		t.Fatalf("reading the server back: %v", err)
	}
	if server.Core != "paper" {
		t.Errorf("a dry run switched the core to %s", server.Core)
	}
}

// Installing empties the mods directory and replaces the core. A running
// server has those files open.
func TestAModpackIsRefusedWhileTheServerRuns(t *testing.T) {
	env := newTestEnv(t)
	fabricPack(t, env, nil)
	env.startServer(testServerID)

	resp := env.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/modpack",
		map[string]any{"project_id": "pack-abc"}, env.modpackToken())
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// A modpack is a set of Java mods and a loader. On a Bedrock server it is not
// a thing that can be installed at all.
func TestBedrockTakesNoModpacks(t *testing.T) {
	env := newTestEnv(t)
	fabricPack(t, env, nil)
	bedrock := seedServer(t, env, "01BEDROCKPACK", "bedrock", 25580)

	resp := env.do(http.MethodPost, "/api/v1/servers/"+bedrock+"/modpack",
		map[string]any{"project_id": "pack-abc"}, env.modpackToken())
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// A mod's version and a pack's version look identical up to the file name, and
// installing a jar as a pack would fail later with a message about a zip.
func TestAJarIsNotAModpack(t *testing.T) {
	env := newTestEnv(t)
	stub := fabricPack(t, env, nil)
	stub.versions[0].Files[0].Name = "some-mod-1.0.0.jar"

	resp := env.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/modpack",
		map[string]any{"project_id": "pack-abc"}, env.modpackToken())
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// The call writes a few hundred files, so files:write governs it as much as
// servers:write does.
func TestInstallingAModpackNeedsBothScopes(t *testing.T) {
	env := newTestEnv(t)
	fabricPack(t, env, nil)

	token := env.mintToken(env.user.ID, []string{ScopeServersRead, ScopeServersWrite})
	resp := env.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/modpack",
		map[string]any{"project_id": "pack-abc"}, token)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// A server with no pack answers "none" rather than failing: the panel shows
// that state and offers the catalogue.
func TestAServerWithoutAModpackSaysSo(t *testing.T) {
	env := newTestEnv(t)
	fabricPack(t, env, nil)

	body := decodeJSON[struct {
		Installed *installedModpack `json:"installed"`
	}](t, env.do(http.MethodGet, "/api/v1/servers/"+testServerID+"/modpack", nil, env.modpackToken()))
	if body.Installed != nil {
		t.Fatalf("installed = %+v, want none", body.Installed)
	}
}
