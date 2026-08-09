package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Arclight publishes several loaders per release, so which one is chosen is a
// decision this provider makes and can make wrongly: a Fabric build handed to
// someone with a Forge modpack looks like a broken server.
func TestArclightPrefersForge(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"tag_name":   "FeudalKings/1.0.1",
			"prerelease": false,
			"assets": []map[string]any{
				{"name": "arclight-fabric-1.21.1-1.0.1-abc.jar", "browser_download_url": "https://example/fabric", "size": 1},
				{"name": "arclight-neoforge-1.21.1-1.0.1-abc.jar", "browser_download_url": "https://example/neoforge", "size": 2},
				{"name": "arclight-forge-1.21.1-1.0.1-abc.jar", "browser_download_url": "https://example/forge", "size": 3},
			},
		}})
	}))
	defer server.Close()

	a := NewArclight(server.Client())
	a.ReleasesURL = server.URL

	build, err := a.Resolve(context.Background(), "1.21.1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if build.URL != "https://example/forge" {
		t.Errorf("url = %q, want the forge build", build.URL)
	}
}

// A release with only Fabric still resolves: refusing would mean offering
// nothing for a version Arclight does publish for.
func TestArclightFallsBackToWhatIsThere(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"tag_name":   "Only/1.0.0",
			"prerelease": false,
			"assets": []map[string]any{
				{"name": "arclight-fabric-1.20.1-1.0.0-abc.jar", "browser_download_url": "https://example/fabric", "size": 1},
			},
		}})
	}))
	defer server.Close()

	a := NewArclight(server.Client())
	a.ReleasesURL = server.URL

	build, err := a.Resolve(context.Background(), "1.20.1")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if build.URL != "https://example/fabric" {
		t.Errorf("url = %q, want the only build there is", build.URL)
	}
}

// Prereleases are skipped: someone picking a version from the panel is not
// asking to test one.
func TestArclightSkipsPrereleases(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"tag_name":   "Testing/2.0.0",
			"prerelease": true,
			"assets": []map[string]any{
				{"name": "arclight-forge-1.21.1-2.0.0-abc.jar", "browser_download_url": "https://example/pre", "size": 1},
			},
		}})
	}))
	defer server.Close()

	a := NewArclight(server.Client())
	a.ReleasesURL = server.URL

	if _, err := a.Resolve(context.Background(), "1.21.1"); err == nil {
		t.Error("a prerelease was offered as an installable version")
	}
}

// Mohist lists a Minecraft version as soon as work on it starts, so "latest"
// has to mean the latest that can actually be installed.
func TestMohistLatestSkipsVersionsWithNoBuilds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/projects/mohist":
			_ = json.NewEncoder(w).Encode(map[string]any{"versions": []string{"1.20.1", "1.21.4"}})
		case "/projects/mohist/1.21.4/builds":
			_ = json.NewEncoder(w).Encode(map[string]any{"builds": []any{}})
		case "/projects/mohist/1.20.1/builds":
			_ = json.NewEncoder(w).Encode(map[string]any{"builds": []map[string]any{
				{"id": "old", "url": "https://example/old", "fileSha256": "aa", "createdAt": 1},
				{"id": "new", "url": "https://example/new", "fileSha256": "bb", "createdAt": 2},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	m := NewMohist(server.Client())
	m.BaseURL = server.URL

	build, err := m.Resolve(context.Background(), "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if build.Version != "1.20.1" {
		t.Errorf("version = %q, want the newest one with a build", build.Version)
	}
	// Newest by timestamp rather than by position: a listing order is not a
	// promise.
	if build.Build != "new" {
		t.Errorf("build = %q, want the newest by timestamp", build.Build)
	}
}

// Asking for a version explicitly still says plainly what is wrong, rather
// than quietly installing a different one.
func TestMohistNamedVersionWithNoBuildsSaysSo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/projects/mohist":
			_ = json.NewEncoder(w).Encode(map[string]any{"versions": []string{"1.20.1", "1.21.4"}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"builds": []any{}})
		}
	}))
	defer server.Close()

	m := NewMohist(server.Client())
	m.BaseURL = server.URL

	_, err := m.Resolve(context.Background(), "1.21.4")
	if err == nil {
		t.Fatal("a version with no builds resolved")
	}
	if !strings.Contains(err.Error(), "1.21.4") {
		t.Errorf("error %q does not name the version asked for", err)
	}
}

// Spigot has no download at all: it is compiled here, and everything
// downstream has to be told that rather than left to find an empty URL.
func TestSpigotResolvesToSomethingToBuild(t *testing.T) {
	s := NewSpigot(nil)
	s.Source = staticVersions{
		{ID: "1.21.4", Channel: ChannelRelease, JavaMajor: 21},
		{ID: "26.3-snapshot-1", Channel: ChannelSnapshot, JavaMajor: 25},
	}

	build, err := s.Resolve(context.Background(), "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !build.NeedsBuild() {
		t.Error("a spigot build does not say it has to be compiled")
	}
	if build.URL != "" {
		t.Errorf("url = %q, want none: there is nothing to download", build.URL)
	}
	if build.Version != "1.21.4" {
		t.Errorf("version = %q, want the newest release", build.Version)
	}

	// BuildTools takes a revision, and a snapshot id is not one.
	if _, err := s.Resolve(context.Background(), "26.3-snapshot-1"); err == nil {
		t.Error("a snapshot was offered as something BuildTools can build")
	}
}

// staticVersions is a provider that answers with a fixed list.
type staticVersions []Version

func (s staticVersions) ID() string       { return "static" }
func (s staticVersions) Name() string     { return "Static" }
func (s staticVersions) Kind() Kind       { return KindServer }
func (s staticVersions) Runtime() Runtime { return RuntimeJava }
func (s staticVersions) Content() Content { return Content{} }

func (s staticVersions) Versions(context.Context) ([]Version, error) { return s, nil }

func (s staticVersions) Resolve(context.Context, string) (*Build, error) {
	return nil, ErrUnknownVersion
}
