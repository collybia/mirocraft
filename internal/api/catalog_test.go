package api

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/collybia/mirocraft/internal/catalog"
	"github.com/collybia/mirocraft/internal/core"
	"github.com/collybia/mirocraft/internal/store"
)

// stubCatalog answers without the network, so the endpoint tests are about the
// endpoints rather than about Modrinth being up.
type stubCatalog struct {
	plan     *catalog.Plan
	planErr  error
	searched catalog.SearchOptions
	// loader and version record what the handler resolved from the server's
	// core, which is the thing most likely to be wrong.
	loader  string
	version string
}

func (s *stubCatalog) Search(_ context.Context, opts catalog.SearchOptions) (*catalog.SearchResult, error) {
	s.searched = opts
	return &catalog.SearchResult{
		Items: []catalog.Project{{ID: "abc", Slug: "test-plugin", Title: "Test Plugin"}},
		Total: 1,
	}, nil
}

func (s *stubCatalog) Project(_ context.Context, idOrSlug string) (*catalog.Project, error) {
	if idOrSlug == "missing" {
		return nil, catalog.ErrNotFound
	}
	return &catalog.Project{ID: "abc", Slug: idOrSlug, Title: "Test Plugin"}, nil
}

func (s *stubCatalog) Versions(context.Context, string, string, string) ([]catalog.Version, error) {
	return []catalog.Version{{ID: "v1", Number: "1.0.0", Channel: "release"}}, nil
}

func (s *stubCatalog) PlanInstall(_ context.Context, _, _, loader, gameVersion string) (*catalog.Plan, error) {
	s.loader, s.version = loader, gameVersion
	if s.planErr != nil {
		return nil, s.planErr
	}
	return s.plan, nil
}

// withCatalog rebuilds the environment's API with a catalogue attached.
//
// newTestEnv leaves both nil, because most of the suite has no business with
// add-ons; these tests are the exception.
func withCatalog(t *testing.T, env *testEnv, stub *stubCatalog) {
	t.Helper()
	env.api.catalog = stub
	env.api.cores = core.DefaultRegistry(nil)
}

// jarServer serves one artifact, so the install path is exercised end to end
// rather than mocked at the download.
func jarServer(t *testing.T, body []byte) *httptest.Server {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server
}

func sha512Of(body []byte) string {
	sum := sha512.Sum512(body)
	return hex.EncodeToString(sum[:])
}

func (e *testEnv) addonToken() string {
	e.t.Helper()
	return e.mintToken(e.user.ID, []string{ScopeServersRead, ScopeFilesRead, ScopeFilesWrite})
}

// seedServer adds another server owned by the fixture user, for the cases that
// need a core other than Paper.
func seedServer(t *testing.T, env *testEnv, id, coreID string, port int) string {
	t.Helper()

	record := &store.Server{
		ID: id, OwnerID: env.user.ID, Name: coreID + "-server", Core: coreID,
		Version: "1.21.4", RAMMb: 1024, Port: port, Dir: "servers/" + id,
	}
	if err := env.db.Servers.Create(context.Background(), record); err != nil {
		t.Fatalf("creating the %s server: %v", coreID, err)
	}
	if err := os.MkdirAll(env.api.serverDir(record), 0o750); err != nil {
		t.Fatalf("creating its directory: %v", err)
	}
	return id
}

// --- search and lookup ---

func TestCatalogSearchPassesTheFilters(t *testing.T) {
	env := newTestEnv(t)
	stub := &stubCatalog{}
	withCatalog(t, env, stub)

	resp := env.do(http.MethodGet,
		"/api/v1/catalog/search?q=worldedit&type=mod&loader=paper&mc=1.21.4&limit=5", nil, env.addonToken())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	if stub.searched.Query != "worldedit" || stub.searched.Loader != "paper" ||
		stub.searched.GameVersion != "1.21.4" || stub.searched.Limit != 5 {
		t.Fatalf("the handler passed %+v", stub.searched)
	}
}

func TestCatalogEndpointsAreOffWithoutARegistry(t *testing.T) {
	env := newTestEnv(t)
	// Deliberately not wired: a node without a catalogue should say so rather
	// than fail obscurely at the first search.
	resp := env.do(http.MethodGet, "/api/v1/catalog/search?q=x", nil, env.addonToken())
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestCatalogProjectNotFound(t *testing.T) {
	env := newTestEnv(t)
	withCatalog(t, env, &stubCatalog{})

	resp := env.do(http.MethodGet, "/api/v1/catalog/projects/missing", nil, env.addonToken())
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// --- what a core accepts ---

func TestServerContentReportsTheLoader(t *testing.T) {
	env := newTestEnv(t)
	withCatalog(t, env, &stubCatalog{})

	body := decodeJSON[contentInfo](t,
		env.do(http.MethodGet, "/api/v1/servers/"+testServerID+"/catalog", nil, env.addonToken()))

	// The fixture server is Paper, which reads plugins/.
	if body.Loader != "paper" || body.Dir != "plugins" {
		t.Fatalf("content = %+v", body)
	}
	if body.Version != "1.21.4" {
		t.Errorf("version = %q", body.Version)
	}
}

// A plugin jar next to a vanilla server is never read, so "installed
// successfully, does nothing" must not be an outcome the panel can produce.
func TestVanillaAcceptsNoAddons(t *testing.T) {
	env := newTestEnv(t)
	withCatalog(t, env, &stubCatalog{})

	vanilla := seedServer(t, env, "01VANILLA", "vanilla", 25570)

	body := decodeJSON[contentInfo](t,
		env.do(http.MethodGet, "/api/v1/servers/"+vanilla+"/catalog", nil, env.addonToken()))
	if body.Loader != "" {
		t.Fatalf("vanilla reported the loader %q", body.Loader)
	}

	resp := env.do(http.MethodPost, "/api/v1/servers/"+vanilla+"/catalog/install",
		map[string]any{"project_id": "abc"}, env.addonToken())
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("installing onto vanilla gave %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// And the list is empty rather than an error: a core with no add-on
	// directory has nothing installed.
	list := decodeJSON[listResponse[installedAddon]](t,
		env.do(http.MethodGet, "/api/v1/servers/"+vanilla+"/installed", nil, env.addonToken()))
	if len(list.Items) != 0 {
		t.Fatalf("vanilla reported %d installed add-ons", len(list.Items))
	}
}

// --- installing ---

func TestInstallDownloadsIntoThePluginDirectory(t *testing.T) {
	env := newTestEnv(t)

	jar := []byte("PK\x03\x04 pretend this is a plugin")
	upstream := jarServer(t, jar)

	stub := &stubCatalog{plan: &catalog.Plan{Files: []catalog.PlannedFile{{
		ProjectID: "abc", ProjectTitle: "Test Plugin", VersionID: "v1",
		FileName: "TestPlugin-1.0.0.jar", URL: upstream.URL,
		SizeBytes: int64(len(jar)), SHA512: sha512Of(jar),
	}}}}
	withCatalog(t, env, stub)

	resp := env.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/catalog/install",
		map[string]any{"project_id": "abc"}, env.addonToken())
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	body := decodeJSON[struct {
		TaskID string        `json:"task_id"`
		Plan   *catalog.Plan `json:"plan"`
	}](t, resp)

	if len(body.Plan.Files) != 1 {
		t.Fatalf("the response carried no plan: %+v", body.Plan)
	}
	env.waitForTask(t, body.TaskID, TaskDone, 30*time.Second)

	// The loader and version come from the server's core, not the request.
	if stub.loader != "paper" || stub.version != "1.21.4" {
		t.Errorf("planned for loader %q version %q", stub.loader, stub.version)
	}

	installed := filepath.Join(env.api.serverDir(env.serverRecord), "plugins", "TestPlugin-1.0.0.jar")
	got, err := os.ReadFile(installed)
	if err != nil {
		t.Fatalf("reading the installed jar: %v", err)
	}
	if string(got) != string(jar) {
		t.Error("the installed file does not match what was served")
	}

	list := decodeJSON[listResponse[installedAddon]](t,
		env.do(http.MethodGet, "/api/v1/servers/"+testServerID+"/installed", nil, env.addonToken()))
	if len(list.Items) != 1 || list.Items[0].Name != "TestPlugin-1.0.0" || !list.Items[0].Enabled {
		t.Fatalf("installed = %+v", list.Items)
	}
}

// Every server that has ever started already has a plugins directory, so this
// is the ordinary case rather than the edge one. It failed: the sandbox
// reports its own "already exists" error, and the install was matching
// os.ErrExist, which never matches it.
func TestInstallIntoAnExistingPluginDirectory(t *testing.T) {
	env := newTestEnv(t)

	pluginDir := filepath.Join(env.api.serverDir(env.serverRecord), "plugins")
	if err := os.MkdirAll(filepath.Join(pluginDir, "bStats"), 0o750); err != nil {
		t.Fatalf("seeding plugins/: %v", err)
	}

	jar := []byte("already have a plugins dir")
	upstream := jarServer(t, jar)
	stub := &stubCatalog{plan: &catalog.Plan{Files: []catalog.PlannedFile{{
		ProjectID: "abc", FileName: "Second-1.0.0.jar", URL: upstream.URL,
		SizeBytes: int64(len(jar)), SHA512: sha512Of(jar),
	}}}}
	withCatalog(t, env, stub)

	resp := env.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/catalog/install",
		map[string]any{"project_id": "abc"}, env.addonToken())
	body := decodeJSON[struct {
		TaskID string `json:"task_id"`
	}](t, resp)

	env.waitForTask(t, body.TaskID, TaskDone, 30*time.Second)

	if _, err := os.Stat(filepath.Join(pluginDir, "Second-1.0.0.jar")); err != nil {
		t.Fatalf("the jar was not installed: %v", err)
	}
	// And what was already there is untouched.
	if _, err := os.Stat(filepath.Join(pluginDir, "bStats")); err != nil {
		t.Errorf("an existing directory was disturbed: %v", err)
	}
}

// A registry is a third party. A jar that does not match its published hash is
// not the jar that was reviewed, and installing it anyway defeats the point of
// publishing one.
func TestInstallRefusesAJarThatFailsItsChecksum(t *testing.T) {
	env := newTestEnv(t)

	upstream := jarServer(t, []byte("this is not what was promised"))
	stub := &stubCatalog{plan: &catalog.Plan{Files: []catalog.PlannedFile{{
		ProjectID: "abc", FileName: "Tampered-1.0.0.jar", URL: upstream.URL,
		SHA512: sha512Of([]byte("the reviewed artifact")),
	}}}}
	withCatalog(t, env, stub)

	resp := env.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/catalog/install",
		map[string]any{"project_id": "abc"}, env.addonToken())
	body := decodeJSON[struct {
		TaskID string `json:"task_id"`
	}](t, resp)

	env.waitForTask(t, body.TaskID, TaskFailed, 30*time.Second)

	// And nothing was left behind for the server to load on its next start.
	pluginDir := filepath.Join(env.api.serverDir(env.serverRecord), "plugins")
	entries, _ := os.ReadDir(pluginDir)
	for _, entry := range entries {
		t.Errorf("a file survived a failed install: %s", entry.Name())
	}
}

// The file name comes from a third-party registry, so it is a path the panel
// did not choose.
func TestInstallRefusesATraversingFileName(t *testing.T) {
	env := newTestEnv(t)

	jar := []byte("evil")
	upstream := jarServer(t, jar)
	stub := &stubCatalog{plan: &catalog.Plan{Files: []catalog.PlannedFile{{
		ProjectID: "abc", FileName: "../../server.jar", URL: upstream.URL,
		SizeBytes: int64(len(jar)), SHA512: sha512Of(jar),
	}}}}
	withCatalog(t, env, stub)

	resp := env.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/catalog/install",
		map[string]any{"project_id": "abc"}, env.addonToken())
	body := decodeJSON[struct {
		TaskID string `json:"task_id"`
	}](t, resp)

	// path.Base reduces it to "server.jar" inside plugins/, so the install
	// succeeds — but it must land in the sandbox, not two levels up.
	env.waitForTask(t, body.TaskID, TaskDone, 30*time.Second)

	dataDir := env.api.dataDir
	if _, err := os.Stat(filepath.Join(dataDir, "server.jar")); err == nil {
		t.Fatal("a jar escaped the server directory")
	}
	if _, err := os.Stat(filepath.Join(env.api.serverDir(env.serverRecord), "server.jar")); err == nil {
		t.Fatal("a jar landed in the server root rather than the add-on directory")
	}
	if _, err := os.Stat(filepath.Join(env.api.serverDir(env.serverRecord), "plugins", "server.jar")); err != nil {
		t.Fatalf("the file did not land in plugins/: %v", err)
	}
}

func TestInstallDryRunDownloadsNothing(t *testing.T) {
	env := newTestEnv(t)

	var served int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served++
		_, _ = w.Write([]byte("jar"))
	}))
	defer upstream.Close()

	stub := &stubCatalog{plan: &catalog.Plan{Files: []catalog.PlannedFile{{
		ProjectID: "abc", FileName: "Plugin.jar", URL: upstream.URL, SizeBytes: 3,
	}}}}
	withCatalog(t, env, stub)

	resp := env.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/catalog/install",
		map[string]any{"project_id": "abc", "dry_run": true}, env.addonToken())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a dry run", resp.StatusCode)
	}
	plan := decodeJSON[catalog.Plan](t, resp)
	if len(plan.Files) != 1 {
		t.Fatalf("the dry run returned no plan: %+v", plan)
	}

	time.Sleep(300 * time.Millisecond)
	if served != 0 {
		t.Fatalf("a dry run downloaded %d files", served)
	}
}

func TestInstallRequiresTheWriteScope(t *testing.T) {
	env := newTestEnv(t)
	withCatalog(t, env, &stubCatalog{plan: &catalog.Plan{}})

	readOnly := env.mintToken(env.user.ID, []string{ScopeServersRead, ScopeFilesRead})
	resp := env.do(http.MethodPost, "/api/v1/servers/"+testServerID+"/catalog/install",
		map[string]any{"project_id": "abc"}, readOnly)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// --- managing what is installed ---

func TestToggleAndDeleteInstalled(t *testing.T) {
	env := newTestEnv(t)
	withCatalog(t, env, &stubCatalog{})
	token := env.addonToken()

	pluginDir := filepath.Join(env.api.serverDir(env.serverRecord), "plugins")
	if err := os.MkdirAll(pluginDir, 0o750); err != nil {
		t.Fatalf("creating plugins/: %v", err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "Thing-1.0.jar"), []byte("jar"), 0o600); err != nil {
		t.Fatalf("writing the jar: %v", err)
	}

	// Off.
	off := decodeJSON[struct {
		File    string `json:"file"`
		Enabled bool   `json:"enabled"`
	}](t, env.do(http.MethodPost,
		"/api/v1/servers/"+testServerID+"/installed/Thing-1.0.jar/toggle", nil, token))

	if off.Enabled || off.File != "Thing-1.0.jar.disabled" {
		t.Fatalf("toggle off = %+v", off)
	}
	if _, err := os.Stat(filepath.Join(pluginDir, "Thing-1.0.jar.disabled")); err != nil {
		t.Fatalf("the jar was not renamed: %v", err)
	}

	list := decodeJSON[listResponse[installedAddon]](t,
		env.do(http.MethodGet, "/api/v1/servers/"+testServerID+"/installed", nil, token))
	if len(list.Items) != 1 || list.Items[0].Enabled {
		t.Fatalf("a disabled add-on is reported as %+v", list.Items)
	}

	// And back on.
	on := decodeJSON[struct {
		File    string `json:"file"`
		Enabled bool   `json:"enabled"`
	}](t, env.do(http.MethodPost,
		"/api/v1/servers/"+testServerID+"/installed/Thing-1.0.jar.disabled/toggle", nil, token))
	if !on.Enabled || on.File != "Thing-1.0.jar" {
		t.Fatalf("toggle on = %+v", on)
	}

	resp := env.do(http.MethodDelete,
		"/api/v1/servers/"+testServerID+"/installed/Thing-1.0.jar", nil, token)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()

	if _, err := os.Stat(filepath.Join(pluginDir, "Thing-1.0.jar")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the jar survived deletion: %v", err)
	}
}

// The name in the path addresses a file, so it is exactly where a traversal
// would be tried.
func TestInstalledEndpointsRefuseNonAddonNames(t *testing.T) {
	env := newTestEnv(t)
	withCatalog(t, env, &stubCatalog{})
	token := env.addonToken()

	for _, name := range []string{
		"..%2F..%2Fserver.properties",
		"server.properties",
		"nested%2Fpath.jar",
		"notajar.txt",
	} {
		for _, call := range []struct{ method, suffix string }{
			{http.MethodDelete, ""},
			{http.MethodPost, "/toggle"},
		} {
			path := "/api/v1/servers/" + testServerID + "/installed/" + name + call.suffix
			resp := env.do(call.method, path, nil, token)
			if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
				t.Errorf("%s %s was accepted", call.method, path)
			}
			_ = resp.Body.Close()
		}
	}

	// Nothing outside the add-on directory was touched.
	if _, err := os.Stat(filepath.Join(env.api.serverDir(env.serverRecord), "server.properties")); err == nil {
		t.Log("server.properties is still present, as it should be")
	}
}
