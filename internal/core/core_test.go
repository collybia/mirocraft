package core

import (
	"context"
	"crypto/sha1"
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
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- java version resolution ---

func TestJavaMajorFor(t *testing.T) {
	tests := []struct {
		version string
		want    int
	}{
		// Java 8 era.
		{"1.8", Java8},
		{"1.8.9", Java8},
		{"1.12.2", Java8},
		{"1.16.5", Java8},

		// Java 17 era. 1.17 officially wants 16, but the panel installs 17
		// rather than carry a runtime nothing else uses.
		{"1.17", Java17},
		{"1.17.1", Java17},
		{"1.18.2", Java17},
		{"1.19.4", Java17},
		{"1.20", Java17},
		{"1.20.4", Java17},

		// 1.20.5 is the boundary, confirmed against Mojang's own manifest.
		{"1.20.5", Java21},
		{"1.20.6", Java21},
		{"1.21", Java21},
		{"1.21.4", Java21},
		{"1.21.11", Java21},

		// The calendar era, which began at 26.1 after 1.21.11. Mojang's
		// manifest reports 25 for these.
		{"26.1", Java25},
		{"26.1.2", Java25},
		{"26.2", Java25},
		{"26.3-snapshot-7", Java25},
		{"27.1", Java25},

		// Retired weekly snapshots are dated, so the year places them.
		{"25w14a", Java21},
		{"24w45a", Java21},
		{"23w31a", Java17},
		{"21w03a", Java17},
		{"18w22c", Java8},

		// Anything unrecognisable is assumed new rather than ancient.
		{"", DefaultJavaMajor},
		{"nonsense", DefaultJavaMajor},
		{"1.21.4-pre1", Java21},
		{"26.2-rc-2", Java25},
	}

	for _, tc := range tests {
		t.Run(tc.version, func(t *testing.T) {
			if got := JavaMajorFor(tc.version); got != tc.want {
				t.Fatalf("JavaMajorFor(%q) = %d, want %d", tc.version, got, tc.want)
			}
		})
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int // sign only
	}{
		{"1.21.4", "1.21.4", 0},
		{"1.21.4", "1.21.3", 1},
		{"1.21.3", "1.21.4", -1},
		{"1.21", "1.20.6", 1},
		{"1.9", "1.10", -1}, // numeric, not lexicographic

		// The calendar era outranks the 1.x era.
		{"26.1", "1.21.11", 1},
		{"1.21.11", "26.1", -1},
		{"26.2", "26.1.2", 1},

		// A release outranks its own pre-releases.
		{"26.2", "26.2-rc-2", 1},
		{"26.2-rc-2", "26.2", -1},
		{"1.21.11", "1.21.11-rc3", 1},

		// Anything with no numeric prefix sorts below everything.
		{"1.21.4", "nonsense", 1},
		{"nonsense", "1.21.4", -1},
	}

	for _, tc := range tests {
		t.Run(tc.a+" vs "+tc.b, func(t *testing.T) {
			got := CompareVersions(tc.a, tc.b)
			if (got > 0) != (tc.want > 0) || (got < 0) != (tc.want < 0) {
				t.Fatalf("CompareVersions(%q, %q) = %d, want sign %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// --- registry ---

type stubProvider struct {
	id   string
	kind Kind
}

func (s stubProvider) ID() string                                  { return s.id }
func (s stubProvider) Name() string                                { return s.id }
func (s stubProvider) Kind() Kind                                  { return s.kind }
func (s stubProvider) Runtime() Runtime                            { return RuntimeJava }
func (s stubProvider) Versions(context.Context) ([]Version, error) { return nil, nil }
func (s stubProvider) Resolve(context.Context, string) (*Build, error) {
	return nil, ErrUnknownVersion
}

func TestRegistry(t *testing.T) {
	r := NewRegistry()
	r.Register(stubProvider{id: "paper", kind: KindServer})
	r.Register(stubProvider{id: "velocity", kind: KindProxy})

	got, err := r.Get("paper")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID() != "paper" {
		t.Fatalf("Get returned %q", got.ID())
	}

	if _, err := r.Get("forge"); !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("Get of an unregistered core = %v, want ErrUnknownProvider", err)
	}

	// List preserves registration order, which is the display order.
	list := r.List()
	if len(list) != 2 || list[0].ID() != "paper" || list[1].ID() != "velocity" {
		t.Fatalf("List = %v, want registration order", list)
	}
}

// Registering the same id twice would let one core silently shadow another.
func TestRegistryRejectsDuplicates(t *testing.T) {
	r := NewRegistry()
	r.Register(stubProvider{id: "paper"})

	defer func() {
		if recover() == nil {
			t.Fatal("registering a duplicate id did not panic")
		}
	}()
	r.Register(stubProvider{id: "paper"})
}

func TestDefaultRegistryHasTheImplementedCores(t *testing.T) {
	r := DefaultRegistry(nil)

	for _, id := range []string{"vanilla", "paper"} {
		if _, err := r.Get(id); err != nil {
			t.Errorf("the default registry has no %q: %v", id, err)
		}
	}
}

// --- vanilla ---

// mojangServer serves a manifest and one version document.
func mojangServer(t *testing.T, jar []byte) (*httptest.Server, *int32) {
	t.Helper()

	sum := sha1.Sum(jar)
	jarSHA := hex.EncodeToString(sum[:])

	var manifestHits int32
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	mux.HandleFunc("/manifest.json", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&manifestHits, 1)
		fmt.Fprintf(w, `{
			"latest": {"release": "1.21.4", "snapshot": "24w45a"},
			"versions": [
				{"id": "24w45a", "type": "snapshot", "url": %q, "releaseTime": "2026-11-06T10:00:00+00:00"},
				{"id": "1.21.4", "type": "release", "url": %q, "releaseTime": "2026-12-03T10:00:00+00:00"},
				{"id": "1.8.9",  "type": "release", "url": %q, "releaseTime": "2015-12-09T10:00:00+00:00"}
			]
		}`, server.URL+"/v/1.21.4.json", server.URL+"/v/1.21.4.json", server.URL+"/v/1.8.9.json")
	})

	mux.HandleFunc("/v/1.21.4.json", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{
			"downloads": {"server": {"sha1": %q, "size": %d, "url": %q}},
			"javaVersion": {"majorVersion": 21}
		}`, jarSHA, len(jar), server.URL+"/jar/server.jar")
	})

	// An old version with no server jar at all.
	mux.HandleFunc("/v/1.8.9.json", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"downloads": {}, "javaVersion": {"majorVersion": 8}}`)
	})

	mux.HandleFunc("/jar/server.jar", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(jar)
	})

	return server, &manifestHits
}

func TestVanillaVersions(t *testing.T) {
	server, _ := mojangServer(t, []byte("fake jar"))

	v := NewVanilla(server.Client())
	v.ManifestURL = server.URL + "/manifest.json"

	versions, err := v.Versions(context.Background())
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("got %d versions, want 3", len(versions))
	}

	// Newest first, regardless of the order upstream used.
	if versions[0].ID != "1.21.4" {
		t.Errorf("first version = %q, want the newest 1.21.4", versions[0].ID)
	}
	if versions[0].Channel != ChannelRelease {
		t.Errorf("channel = %q, want release", versions[0].Channel)
	}

	for _, v := range versions {
		if v.ID == "24w45a" && v.Channel != ChannelSnapshot {
			t.Errorf("24w45a is marked %q, want snapshot", v.Channel)
		}
		if v.ID == "1.8.9" && v.JavaMajor != Java8 {
			t.Errorf("1.8.9 wants Java %d, expected 8", v.JavaMajor)
		}
	}
}

func TestVanillaResolve(t *testing.T) {
	jar := []byte("pretend this is a server jar")
	server, _ := mojangServer(t, jar)

	v := NewVanilla(server.Client())
	v.ManifestURL = server.URL + "/manifest.json"

	build, err := v.Resolve(context.Background(), "1.21.4")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if build.Core != "vanilla" || build.Version != "1.21.4" {
		t.Errorf("build = %+v", build)
	}
	if build.Algorithm != AlgoSHA1 || build.Checksum == "" {
		t.Errorf("no usable checksum: %+v", build)
	}
	if build.SizeBytes != int64(len(jar)) {
		t.Errorf("size = %d, want %d", build.SizeBytes, len(jar))
	}
	// The manifest states 21, and the manifest wins over the local table.
	if build.JavaMajor != 21 {
		t.Errorf("java major = %d, want the manifest's 21", build.JavaMajor)
	}
}

// An empty version must mean the latest release, never the latest snapshot.
func TestVanillaResolveDefaultsToTheLatestRelease(t *testing.T) {
	server, _ := mojangServer(t, []byte("jar"))

	v := NewVanilla(server.Client())
	v.ManifestURL = server.URL + "/manifest.json"

	build, err := v.Resolve(context.Background(), "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if build.Version != "1.21.4" {
		t.Fatalf("default version = %q, want the latest release", build.Version)
	}
}

func TestVanillaResolveUnknownVersion(t *testing.T) {
	server, _ := mojangServer(t, []byte("jar"))

	v := NewVanilla(server.Client())
	v.ManifestURL = server.URL + "/manifest.json"

	if _, err := v.Resolve(context.Background(), "1.99.9"); !errors.Is(err, ErrUnknownVersion) {
		t.Fatalf("Resolve of an unknown version = %v, want ErrUnknownVersion", err)
	}
}

// Versions that predate a published server jar must be reported, not returned
// with an empty URL that fails much later at download time.
func TestVanillaResolveVersionWithoutAServerJar(t *testing.T) {
	server, _ := mojangServer(t, []byte("jar"))

	v := NewVanilla(server.Client())
	v.ManifestURL = server.URL + "/manifest.json"

	if _, err := v.Resolve(context.Background(), "1.8.9"); !errors.Is(err, ErrUnknownVersion) {
		t.Fatalf("Resolve = %v, want ErrUnknownVersion", err)
	}
}

// The manifest is fetched once and reused, so listing versions repeatedly
// does not hammer Mojang.
func TestVanillaCachesTheManifest(t *testing.T) {
	server, hits := mojangServer(t, []byte("jar"))

	v := NewVanilla(server.Client())
	v.ManifestURL = server.URL + "/manifest.json"

	for i := 0; i < 5; i++ {
		if _, err := v.Versions(context.Background()); err != nil {
			t.Fatalf("Versions: %v", err)
		}
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("the manifest was fetched %d times, want 1", got)
	}
}

// When the cache expires and upstream is down, a stale manifest is better
// than an error: the panel stays usable and versions do not vanish.
func TestVanillaFallsBackToTheStaleManifest(t *testing.T) {
	server, _ := mojangServer(t, []byte("jar"))

	v := NewVanilla(server.Client())
	v.ManifestURL = server.URL + "/manifest.json"

	if _, err := v.Versions(context.Background()); err != nil {
		t.Fatalf("Versions: %v", err)
	}

	// Expire the cache and break upstream.
	v.now = func() time.Time { return time.Now().Add(2 * manifestTTL) }
	v.ManifestURL = server.URL + "/gone"

	versions, err := v.Versions(context.Background())
	if err != nil {
		t.Fatalf("Versions with upstream down: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("got %d versions from the stale cache, want 3", len(versions))
	}
}

func TestVanillaWithoutACachedManifestReportsTheError(t *testing.T) {
	server, _ := mojangServer(t, []byte("jar"))

	v := NewVanilla(server.Client())
	v.ManifestURL = server.URL + "/does-not-exist"

	if _, err := v.Versions(context.Background()); err == nil {
		t.Fatal("Versions succeeded with no manifest and no cache")
	}
}

// --- paper ---

// paperServer mirrors the shape fill.papermc.io/v3 actually returns, checked
// against the live API rather than assumed.
func paperServer(t *testing.T, jar []byte) (*httptest.Server, *int32) {
	t.Helper()

	sum := sha256.Sum256(jar)
	jarSHA := hex.EncodeToString(sum[:])

	var projectHits int32
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	mux.HandleFunc("/projects/paper", func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&projectHits, 1)
		// Versions arrive grouped by family, in a JSON object whose key order
		// Go does not preserve.
		fmt.Fprint(w, `{
			"project": {"id": "paper", "name": "Paper"},
			"versions": {
				"26.2": ["26.2", "26.2-rc-2"],
				"26.1": ["26.1.2"],
				"1.21": ["1.21.11", "1.21.4"]
			}
		}`)
	})

	mux.HandleFunc("/projects/paper/versions/26.2/builds/latest",
		func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, `{
				"id": 111,
				"time": "2026-08-07T16:16:19Z",
				"channel": "STABLE",
				"downloads": {
					"server:default": {
						"name": "paper-26.2-111.jar",
						"size": %d,
						"url": %q,
						"checksums": {"sha256": %q}
					},
					"server:mojmap": {
						"name": "paper-26.2-111-mojmap.jar",
						"size": 1,
						"url": "http://example.invalid/wrong.jar",
						"checksums": {"sha256": "deadbeef"}
					}
				}
			}`, len(jar), server.URL+"/objects/paper-26.2-111.jar", jarSHA)
		})

	mux.HandleFunc("/projects/paper/versions/1.21.4/builds/latest",
		func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, `{
				"id": 7, "channel": "STABLE",
				"downloads": {"server:default": {
					"name": "paper-1.21.4-7.jar", "size": %d, "url": %q,
					"checksums": {"sha256": %q}
				}}
			}`, len(jar), server.URL+"/objects/paper-1.21.4-7.jar", jarSHA)
		})

	// A version the project lists but has not built yet.
	mux.HandleFunc("/projects/paper/versions/26.1.2/builds/latest",
		func(w http.ResponseWriter, r *http.Request) {
			http.NotFound(w, r)
		})

	mux.HandleFunc("/objects/", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(jar)
	})

	return server, &projectHits
}

func TestPaperVersionsAreNewestFirst(t *testing.T) {
	server, _ := paperServer(t, []byte("jar"))

	p := NewPaper(server.Client())
	p.BaseURL = server.URL

	versions, err := p.Versions(context.Background())
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}

	// Grouped families flattened and ordered, with each release above its own
	// release candidate.
	want := []string{"26.2", "26.2-rc-2", "26.1.2", "1.21.11", "1.21.4"}
	if len(versions) != len(want) {
		t.Fatalf("got %d versions, want %d", len(versions), len(want))
	}
	for i, id := range want {
		if versions[i].ID != id {
			t.Fatalf("version %d = %q, want %q", i, versions[i].ID, id)
		}
	}

	// The ordering must not depend on Go's random map iteration.
	for i := 0; i < 20; i++ {
		again, err := p.Versions(context.Background())
		if err != nil {
			t.Fatalf("Versions: %v", err)
		}
		for j := range want {
			if again[j].ID != want[j] {
				t.Fatalf("ordering changed between calls: %q at %d", again[j].ID, j)
			}
		}
	}
}

func TestPaperMarksPreReleases(t *testing.T) {
	server, _ := paperServer(t, []byte("jar"))

	p := NewPaper(server.Client())
	p.BaseURL = server.URL

	versions, err := p.Versions(context.Background())
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}

	for _, v := range versions {
		want := ChannelRelease
		if v.ID == "26.2-rc-2" {
			want = ChannelSnapshot
		}
		if v.Channel != want {
			t.Errorf("%s is marked %q, want %q", v.ID, v.Channel, want)
		}
	}
}

func TestPaperResolvePicksTheLatestBuild(t *testing.T) {
	server, _ := paperServer(t, []byte("jar"))

	p := NewPaper(server.Client())
	p.BaseURL = server.URL

	build, err := p.Resolve(context.Background(), "26.2")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if build.Build != "111" {
		t.Errorf("build = %q, want 111", build.Build)
	}
	if build.FileName != "paper-26.2-111.jar" {
		t.Errorf("file name = %q", build.FileName)
	}
	if build.Algorithm != AlgoSHA256 || build.Checksum == "" {
		t.Errorf("no usable checksum: %+v", build)
	}
	if build.JavaMajor != Java25 {
		t.Errorf("java major = %d, want 25", build.JavaMajor)
	}
	// Paper also publishes a mojang-mapped jar, which is not what an operator
	// wants to run.
	if strings.Contains(build.FileName, "mojmap") {
		t.Errorf("picked the mojang-mapped artifact: %q", build.FileName)
	}
}

// "Latest" must mean the latest release, not the release candidate that sorts
// above it.
func TestPaperResolveDefaultsToTheNewestRelease(t *testing.T) {
	server, _ := paperServer(t, []byte("jar"))

	p := NewPaper(server.Client())
	p.BaseURL = server.URL

	build, err := p.Resolve(context.Background(), "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if build.Version != "26.2" {
		t.Fatalf("default version = %q, want the newest release", build.Version)
	}
}

func TestPaperResolveUnknownVersion(t *testing.T) {
	server, _ := paperServer(t, []byte("jar"))

	p := NewPaper(server.Client())
	p.BaseURL = server.URL

	if _, err := p.Resolve(context.Background(), "1.7.10"); !errors.Is(err, ErrUnknownVersion) {
		t.Fatalf("Resolve = %v, want ErrUnknownVersion", err)
	}
}

// A version the project lists but has not built yet must say so clearly.
func TestPaperResolveVersionWithNoBuilds(t *testing.T) {
	server, _ := paperServer(t, []byte("jar"))

	p := NewPaper(server.Client())
	p.BaseURL = server.URL

	if _, err := p.Resolve(context.Background(), "26.1.2"); !errors.Is(err, ErrNoBuilds) {
		t.Fatalf("Resolve = %v, want ErrNoBuilds", err)
	}
}

func TestPaperCachesTheProject(t *testing.T) {
	server, hits := paperServer(t, []byte("jar"))

	p := NewPaper(server.Client())
	p.BaseURL = server.URL

	for i := 0; i < 4; i++ {
		if _, err := p.Versions(context.Background()); err != nil {
			t.Fatalf("Versions: %v", err)
		}
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("the project was fetched %d times, want 1", got)
	}
}

func TestIsRelease(t *testing.T) {
	releases := []string{"1.21.4", "1.21.11", "26.1", "26.1.2", "26.2"}
	for _, v := range releases {
		if !IsRelease(v) {
			t.Errorf("IsRelease(%q) = false, want true", v)
		}
	}

	preReleases := []string{"26.2-rc-2", "1.21.11-rc3", "1.21.9-pre4", "26.3-snapshot-7", "24w45a", "", "nonsense"}
	for _, v := range preReleases {
		if IsRelease(v) {
			t.Errorf("IsRelease(%q) = true, want false", v)
		}
	}
}

// --- downloading ---

func TestDownloadVerifiesAndCaches(t *testing.T) {
	jar := []byte("pretend this is a paper jar")
	server, _ := paperServer(t, jar)

	p := NewPaper(server.Client())
	p.BaseURL = server.URL

	build, err := p.Resolve(context.Background(), "1.21.4")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	d := NewDownloader(t.TempDir(), discardLogger())
	d.Client = server.Client()

	path, err := d.Fetch(context.Background(), build)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the cached jar: %v", err)
	}
	if string(got) != string(jar) {
		t.Fatalf("cached content = %q, want the downloaded jar", got)
	}

	// A second fetch must be served from the cache, without touching the
	// network at all.
	d.Client = &http.Client{Transport: refusingTransport{t}}
	again, err := d.Fetch(context.Background(), build)
	if err != nil {
		t.Fatalf("second Fetch: %v", err)
	}
	if again != path {
		t.Fatalf("second Fetch returned %q, want the cached %q", again, path)
	}
}

// refusingTransport fails any request, proving the cache was used.
type refusingTransport struct{ t *testing.T }

func (r refusingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.t.Errorf("the cache was bypassed: request to %s", req.URL)
	return nil, errors.New("network access is not allowed in this phase of the test")
}

// A jar whose checksum does not match must be rejected, and must not be left
// in the cache where the next start would pick it up.
func TestDownloadRejectsABadChecksum(t *testing.T) {
	jar := []byte("the real jar")
	server, _ := paperServer(t, jar)

	p := NewPaper(server.Client())
	p.BaseURL = server.URL

	build, err := p.Resolve(context.Background(), "1.21.4")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	build.Checksum = strings.Repeat("0", 64) // a valid-looking but wrong sum

	dir := t.TempDir()
	d := NewDownloader(dir, discardLogger())
	d.Client = server.Client()

	if _, err := d.Fetch(context.Background(), build); !errors.Is(err, ErrChecksum) {
		t.Fatalf("Fetch = %v, want ErrChecksum", err)
	}

	if _, err := os.Stat(d.CachePath(build)); !os.IsNotExist(err) {
		t.Fatal("the rejected download was left in the cache")
	}
}

// A truncated download must fail on the declared size, not be cached.
func TestDownloadRejectsAShortBody(t *testing.T) {
	full := []byte("0123456789")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(full[:4])
	}))
	t.Cleanup(server.Close)

	sum := sha256.Sum256(full)
	build := &Build{
		Core: "paper", Version: "1.21.4", Build: "1",
		URL: server.URL, FileName: "paper.jar",
		Checksum: hex.EncodeToString(sum[:]), Algorithm: AlgoSHA256,
		SizeBytes: int64(len(full)),
	}

	d := NewDownloader(t.TempDir(), discardLogger())
	d.Client = server.Client()

	if _, err := d.Fetch(context.Background(), build); err == nil {
		t.Fatal("Fetch accepted a truncated download")
	}
	if _, err := os.Stat(d.CachePath(build)); !os.IsNotExist(err) {
		t.Fatal("the truncated download was cached")
	}
}

// A cached file that has since been corrupted must be refetched rather than
// handed to the JVM, where it would surface as an unexplained crash.
func TestDownloadRefetchesACorruptedCacheEntry(t *testing.T) {
	jar := []byte("pretend this is a paper jar")
	server, _ := paperServer(t, jar)

	p := NewPaper(server.Client())
	p.BaseURL = server.URL

	build, err := p.Resolve(context.Background(), "1.21.4")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	d := NewDownloader(t.TempDir(), discardLogger())
	d.Client = server.Client()

	path, err := d.Fetch(context.Background(), build)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	// Corrupt it in place, keeping the length so only the checksum catches it.
	corrupted := make([]byte, len(jar))
	copy(corrupted, jar)
	corrupted[0] ^= 0xFF
	if err := os.WriteFile(path, corrupted, 0o600); err != nil {
		t.Fatalf("corrupting the cache: %v", err)
	}

	if _, err := d.Fetch(context.Background(), build); err != nil {
		t.Fatalf("Fetch after corruption: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if string(got) != string(jar) {
		t.Fatal("the corrupted file was reused instead of refetched")
	}
}

// An upstream error must not leave a partial file behind.
func TestDownloadOnAnErrorLeavesNothingBehind(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	t.Cleanup(server.Close)

	build := &Build{
		Core: "paper", Version: "1.21.4", Build: "1",
		URL: server.URL, FileName: "paper.jar",
	}

	dir := t.TempDir()
	d := NewDownloader(dir, discardLogger())
	d.Client = server.Client()

	if _, err := d.Fetch(context.Background(), build); err == nil {
		t.Fatal("Fetch succeeded against a 404")
	}

	entries, err := os.ReadDir(filepath.Dir(d.CachePath(build)))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading the cache dir: %v", err)
	}
	for _, entry := range entries {
		t.Errorf("the failed download left %q behind", entry.Name())
	}
}

// Upstream ids end up in a filesystem path, so they must not be able to
// escape the cache directory.
func TestCachePathCannotEscapeTheCacheDirectory(t *testing.T) {
	d := NewDownloader(filepath.Join("data", "cache"), discardLogger())

	hostile := &Build{
		Core:     "../../etc",
		Version:  "../..",
		Build:    "..",
		FileName: "../../../etc/passwd",
	}

	path := d.CachePath(hostile)

	// The invariant is not "the string contains no dots" — version ids and
	// jar names are full of them, and `.._etc` is an ordinary directory name.
	// What must never happen is a path COMPONENT that is "." or "..", since
	// only those actually walk up the tree.
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." || part == "." {
			t.Fatalf("cache path %q contains a traversal component %q", path, part)
		}
	}

	root, err := filepath.Abs(d.Dir)
	if err != nil {
		t.Fatalf("resolving the cache root: %v", err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("resolving the cache path: %v", err)
	}
	if !strings.HasPrefix(abs, root) {
		t.Fatalf("cache path %q escapes the cache root %q", abs, root)
	}
}

func TestSanitizePathPart(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"1.21.4", "1.21.4"},
		{"paper-1.21.4-102.jar", "paper-1.21.4-102.jar"},
		{"24w45a", "24w45a"},
		{"../etc", ".._etc"}, // a dot is legal in a name; only a bare ".." is not
		{"a/b", "a_b"},
		{"a\\b", "a_b"},
		{"..", ""},
		{".", ""},
		{"", ""},
		{"   ", ""},
	}

	for _, tc := range tests {
		if got := sanitizePathPart(tc.in); got != tc.want {
			t.Errorf("sanitizePathPart(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A build with no published checksum still downloads — some cores publish
// none — but the fact is surfaced rather than silently treated as verified.
func TestBuildVerifiable(t *testing.T) {
	if (&Build{Checksum: "abc", Algorithm: AlgoSHA1}).Verifiable() != true {
		t.Error("a build with a checksum reports itself unverifiable")
	}
	if (&Build{Checksum: "abc"}).Verifiable() != false {
		t.Error("a checksum with no algorithm counts as verifiable")
	}
	if (&Build{Algorithm: AlgoSHA1}).Verifiable() != false {
		t.Error("an algorithm with no checksum counts as verifiable")
	}
}

func TestDownloadWithoutAChecksumStillWorks(t *testing.T) {
	body := []byte("a jar nobody signed")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)

	build := &Build{Core: "custom", Version: "1.0", URL: server.URL, FileName: "server.jar"}

	d := NewDownloader(t.TempDir(), discardLogger())
	d.Client = server.Client()

	path, err := d.Fetch(context.Background(), build)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("content = %q", got)
	}
}

func TestUnsupportedChecksumAlgorithmIsRejected(t *testing.T) {
	if _, err := newHasher("md5"); err == nil {
		t.Fatal("newHasher accepted md5")
	}
}
