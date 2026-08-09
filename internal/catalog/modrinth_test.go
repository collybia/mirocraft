package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeUpstream stands in for Modrinth, answering the shapes the real API was
// observed to return.
type fakeUpstream struct {
	*httptest.Server

	requests atomic.Int32
	// lastQuery records what the client asked for, so the facets it builds can
	// be checked rather than assumed.
	lastQuery url.Values
	status    int
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	t.Helper()
	fake := &fakeUpstream{}

	fake.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.requests.Add(1)
		fake.lastQuery = r.URL.Query()

		if fake.status != 0 {
			w.WriteHeader(fake.status)
			_, _ = w.Write([]byte(`{"error":"upstream said no"}`))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/search":
			_, _ = w.Write([]byte(searchPayload))

		case r.URL.Path == "/project/essentialsx":
			_, _ = w.Write([]byte(projectPayload))

		case r.URL.Path == "/project/essentialsx/version",
			r.URL.Path == "/project/hXiIvTyT/version":
			_, _ = w.Write([]byte(versionsPayload))

		case r.URL.Path == "/project/needs-lib/version":
			_, _ = w.Write([]byte(dependentVersionsPayload))

		case r.URL.Path == "/project/the-lib/version":
			_, _ = w.Write([]byte(libraryVersionsPayload))

		case knownProjects[strings.TrimPrefix(r.URL.Path, "/project/")]:
			// A detail lookup for a project the fixtures know: enough to give
			// the planner a title. Anything else falls through to the 404
			// below, because a registry that answered for every id would hide
			// exactly the case the not-found handling exists for.
			_, _ = w.Write([]byte(`{"id":"x","slug":"x","title":"X","project_type":"mod"}`))

		case r.URL.Path == "/version/SKQwLLoQ":
			var payload []json.RawMessage
			_ = json.Unmarshal([]byte(versionsPayload), &payload)
			_, _ = w.Write(payload[0])

		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
		}
	}))

	t.Cleanup(fake.Close)
	return fake
}

func (f *fakeUpstream) client() *Client {
	c := New(f.Client())
	c.BaseURL = f.URL
	return c
}

// knownProjects are the ids the fixtures cover. Everything else is answered
// with a 404, the way the real registry does.
var knownProjects = map[string]bool{
	"hXiIvTyT": true, "needs-lib": true, "the-lib": true,
	"nice-to-have": true, "bundled": true,
}

// Payloads copied in shape from the live API, trimmed to the fields the client
// reads. Real ones were captured while writing this — guessing them would test
// the guess rather than the code.
const searchPayload = `{
  "hits": [
    {"project_id":"hXiIvTyT","slug":"essentialsx","title":"EssentialsX",
     "description":"The essential plugin suite","project_type":"mod","downloads":702892,
     "icon_url":"https://cdn.modrinth.com/icon.webp",
     "categories":["bukkit","paper","spigot","utility"],
     "server_side":"required","client_side":"unsupported","license":"GPL-3.0-only"}
  ],
  "offset": 0,
  "total_hits": 108
}`

const projectPayload = `{
  "id":"hXiIvTyT","slug":"essentialsx","title":"EssentialsX",
  "description":"The essential plugin suite","body":"# EssentialsX\nlong text",
  "project_type":"mod","downloads":702892,"icon_url":"https://cdn.modrinth.com/icon.webp",
  "categories":["bukkit","paper","spigot"],"loaders":["bukkit","paper","spigot"],
  "server_side":"required","client_side":"unsupported",
  "license":{"id":"GPL-3.0-only","name":"GNU General Public License v3.0 only"}
}`

const versionsPayload = `[
  {"id":"SKQwLLoQ","project_id":"hXiIvTyT","name":"2.21.0","version_number":"2.21.0",
   "version_type":"release","loaders":["bukkit","paper","spigot"],
   "game_versions":["1.21.4"],"date_published":"2025-06-09T10:15:42Z",
   "files":[
     {"filename":"EssentialsX-2.21.0-sources.jar","url":"https://cdn/sources.jar","size":100,
      "primary":false,"hashes":{"sha1":"aaa","sha512":"bbb"}},
     {"filename":"EssentialsX-2.21.0.jar","url":"https://cdn/EssentialsX-2.21.0.jar","size":4605977,
      "primary":true,"hashes":{"sha1":"ccc","sha512":"ddd"}}
   ],
   "dependencies":[]}
]`

// A project that needs a library, plus an optional extra nobody asked for.
const dependentVersionsPayload = `[
  {"id":"v-alpha","project_id":"needs-lib","name":"3.0.0-alpha","version_number":"3.0.0-alpha",
   "version_type":"alpha","loaders":["paper"],"game_versions":["1.21.4"],
   "date_published":"2026-01-02T00:00:00Z",
   "files":[{"filename":"needs-lib-3.0.0-alpha.jar","url":"https://cdn/alpha.jar","size":10,
             "primary":true,"hashes":{"sha512":"a1"}}],
   "dependencies":[]},
  {"id":"v-release","project_id":"needs-lib","name":"2.0.0","version_number":"2.0.0",
   "version_type":"release","loaders":["paper"],"game_versions":["1.21.4"],
   "date_published":"2026-01-01T00:00:00Z",
   "files":[{"filename":"needs-lib-2.0.0.jar","url":"https://cdn/release.jar","size":20,
             "primary":true,"hashes":{"sha512":"a2"}}],
   "dependencies":[
     {"project_id":"the-lib","version_id":null,"dependency_type":"required"},
     {"project_id":"nice-to-have","version_id":null,"dependency_type":"optional"},
     {"project_id":"bundled","version_id":null,"dependency_type":"embedded"}
   ]}
]`

const libraryVersionsPayload = `[
  {"id":"lib-1","project_id":"the-lib","name":"1.4.0","version_number":"1.4.0",
   "version_type":"release","loaders":["paper"],"game_versions":["1.21.4"],
   "date_published":"2026-01-01T00:00:00Z",
   "files":[{"filename":"the-lib-1.4.0.jar","url":"https://cdn/lib.jar","size":30,
             "primary":true,"hashes":{"sha512":"a3"}}],
   "dependencies":[]}
]`

// --- search ---

func TestSearchBuildsTheFacetsModrinthExpects(t *testing.T) {
	fake := newFakeUpstream(t)
	client := fake.client()

	result, err := client.Search(context.Background(), SearchOptions{
		Query: "essentials", Type: "mod", Loader: "paper", GameVersion: "1.21.4",
	})
	if err != nil {
		t.Fatalf("searching: %v", err)
	}

	var facets [][]string
	if err := json.Unmarshal([]byte(fake.lastQuery.Get("facets")), &facets); err != nil {
		t.Fatalf("the facets are not JSON: %v", err)
	}

	flat := map[string]bool{}
	for _, group := range facets {
		for _, facet := range group {
			flat[facet] = true
		}
	}

	// The loader goes in as a category, not a "loaders" facet: search indexes
	// it that way, which is only knowable by asking the real API.
	for _, want := range []string{"project_type:mod", "categories:paper", "versions:1.21.4"} {
		if !flat[want] {
			t.Errorf("the facets are missing %q: %v", want, facets)
		}
	}
	// A purely client-side mod on a server does nothing at all, so it is
	// filtered out rather than offered and then quietly ignored.
	if !flat["server_side:required"] || !flat["server_side:optional"] {
		t.Errorf("the search does not filter to server-side projects: %v", facets)
	}

	if len(result.Items) != 1 || result.Items[0].Slug != "essentialsx" {
		t.Fatalf("items = %+v", result.Items)
	}
	if result.Total != 108 {
		t.Errorf("total = %d", result.Total)
	}
	if result.Items[0].Downloads != 702892 || result.Items[0].ServerSide != "required" {
		t.Errorf("hit = %+v", result.Items[0])
	}
}

func TestSearchIsCached(t *testing.T) {
	fake := newFakeUpstream(t)
	client := fake.client()

	for i := 0; i < 3; i++ {
		if _, err := client.Search(context.Background(), SearchOptions{Query: "same"}); err != nil {
			t.Fatalf("searching: %v", err)
		}
	}

	// Modrinth's guidelines ask for exactly this, and a panel that asks again
	// on every keystroke gets everyone using the API rate-limited.
	if got := fake.requests.Load(); got != 1 {
		t.Fatalf("three identical searches made %d requests", got)
	}

	if _, err := client.Search(context.Background(), SearchOptions{Query: "different"}); err != nil {
		t.Fatalf("searching: %v", err)
	}
	if got := fake.requests.Load(); got != 2 {
		t.Fatalf("a different search made %d requests in total", got)
	}
}

func TestProjectAndVersions(t *testing.T) {
	client := newFakeUpstream(t).client()
	ctx := context.Background()

	project, err := client.Project(ctx, "essentialsx")
	if err != nil {
		t.Fatalf("project: %v", err)
	}
	if project.ID != "hXiIvTyT" || project.Title != "EssentialsX" {
		t.Fatalf("project = %+v", project)
	}
	// The licence arrives as an object upstream and is flattened to its id:
	// the panel shows "GPL-3.0-only", not a nested document.
	if project.License != "GPL-3.0-only" {
		t.Errorf("license = %q", project.License)
	}
	if project.Body == "" {
		t.Error("the detail lookup returned no body")
	}

	versions, err := client.Versions(ctx, "essentialsx", "paper", "1.21.4")
	if err != nil {
		t.Fatalf("versions: %v", err)
	}
	if len(versions) != 1 {
		t.Fatalf("versions = %d", len(versions))
	}
	if versions[0].PublishedAt.IsZero() {
		t.Error("the version has no publication date")
	}
}

// A version can ship sources alongside the artifact, and installing the wrong
// one gives a jar the server cannot load.
func TestPrimaryFileIsTheArtifact(t *testing.T) {
	client := newFakeUpstream(t).client()

	versions, err := client.Versions(context.Background(), "essentialsx", "", "")
	if err != nil {
		t.Fatalf("versions: %v", err)
	}

	file, ok := versions[0].PrimaryFile()
	if !ok {
		t.Fatal("no primary file")
	}
	if file.Name != "EssentialsX-2.21.0.jar" {
		t.Fatalf("primary file = %q, want the artifact rather than the sources", file.Name)
	}
	if file.SHA512 != "ddd" {
		t.Errorf("sha512 = %q", file.SHA512)
	}
}

func TestPrimaryFileFallsBackToTheOnlyFile(t *testing.T) {
	// A few older versions predate the primary flag, and refusing to install
	// them would be a worse answer than taking the one file they publish.
	version := Version{Files: []File{{Name: "old.jar", URL: "https://cdn/old.jar"}}}

	file, ok := version.PrimaryFile()
	if !ok || file.Name != "old.jar" {
		t.Fatalf("file = %+v, ok = %v", file, ok)
	}
}

// --- errors ---

func TestNotFoundIsDistinguishable(t *testing.T) {
	client := newFakeUpstream(t).client()

	_, err := client.Project(context.Background(), "no-such-project")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound so the API can answer 404", err)
	}
}

func TestRateLimitIsDistinguishable(t *testing.T) {
	fake := newFakeUpstream(t)
	fake.status = http.StatusTooManyRequests

	_, err := fake.client().Search(context.Background(), SearchOptions{Query: "x"})
	// An operator told "rate limited" waits; one told "500" goes looking for a
	// bug in the panel.
	if !errors.Is(err, ErrRateLimit) {
		t.Fatalf("error = %v, want ErrRateLimit", err)
	}
}

func TestUpstreamFailureIsReported(t *testing.T) {
	fake := newFakeUpstream(t)
	fake.status = http.StatusBadGateway

	_, err := fake.client().Search(context.Background(), SearchOptions{Query: "x"})
	if err == nil {
		t.Fatal("a 502 was treated as success")
	}
	if !strings.Contains(err.Error(), "upstream said no") {
		t.Errorf("error = %v, want the upstream body kept", err)
	}
}

// --- the cache itself ---

func TestCacheExpires(t *testing.T) {
	c := newCache()
	c.set("key", "value", 20*time.Millisecond)

	if _, ok := c.get("key"); !ok {
		t.Fatal("a fresh entry was not returned")
	}
	time.Sleep(40 * time.Millisecond)
	if _, ok := c.get("key"); ok {
		t.Fatal("an expired entry was returned")
	}
}

// A search box makes a new key per keystroke, so an unbounded cache grows for
// as long as the daemon runs.
func TestCacheIsBounded(t *testing.T) {
	c := newCache()
	for i := 0; i < maxEntries*3; i++ {
		c.set(string(rune(i))+"-key", i, time.Hour)
	}
	if c.Len() > maxEntries {
		t.Fatalf("the cache holds %d entries, over the %d bound", c.Len(), maxEntries)
	}
}
