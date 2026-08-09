//go:build live

// Checks the providers against the real upstream APIs.
//
// Behind a build tag because it needs the network and downloads a real server
// jar, which is far too slow and too flaky for the normal suite. It exists
// because the fixtures in core_test.go can only prove the code handles the
// shape it was written against — and upstream changes that shape without
// asking. Both of the assumptions this package started with turned out to be
// wrong when first run against reality: PaperMC's v2 API had been sunset, and
// Minecraft had moved from 1.x to calendar versioning with a new Java
// requirement.
//
// Run it when a core stops working, and before releasing:
//
//	go test -tags live -run TestLive -v ./internal/core/
package core

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

func liveContext(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

// TestLiveProvidersResolve checks that every registered core can still list
// versions and resolve a build from its real API.
func TestLiveProvidersResolve(t *testing.T) {
	ctx := liveContext(t)

	for _, p := range DefaultRegistry(nil).List() {
		t.Run(p.ID(), func(t *testing.T) {
			versions, err := p.Versions(ctx)
			if err != nil {
				t.Fatalf("Versions: %v", err)
			}
			if len(versions) == 0 {
				t.Fatal("the provider lists no versions at all")
			}

			build, err := p.Resolve(ctx, "")
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}

			// A core that is compiled here has no URL by design: the artifact
			// does not exist anywhere until this machine makes it.
			if build.FileName == "" || (build.URL == "" && !build.NeedsBuild()) {
				t.Fatalf("incomplete build: %+v", build)
			}
			// Not every project publishes one. Fabric's server endpoint and
			// Pufferfish's CI do not, and Build.Verifiable exists to say so
			// rather than to pretend otherwise — so this is recorded, not
			// failed. What would be worth failing is a provider that used to
			// publish a checksum and stopped, and that shows up as a changed
			// line in this log.
			if !build.Verifiable() {
				t.Logf("%s publishes no checksum for %s; downloads are trusted to TLS alone",
					p.ID(), build.Version)
			}
			// A native server needs no Java at all, and zero is the honest
			// answer there: the runner reads this to pick an image, and a
			// number invented for a binary that does not use one would send
			// it to the wrong image.
			if p.Runtime() == RuntimeJava && build.JavaMajor <= 0 {
				t.Errorf("no Java requirement resolved for %s %s", p.ID(), build.Version)
			}

			t.Logf("%s %s build=%q java=%d %s=%s",
				build.Core, build.Version, build.Build, build.JavaMajor,
				build.Algorithm, build.Checksum)
		})
	}
}

// TestLiveDownloadVerifies downloads the real artifact and checks it against
// the checksum upstream published. This is the only test that proves the
// published checksums and the published bytes actually agree.
func TestLiveDownloadVerifies(t *testing.T) {
	ctx := liveContext(t)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	d := NewDownloader(t.TempDir(), log)

	for _, p := range DefaultRegistry(nil).List() {
		t.Run(p.ID(), func(t *testing.T) {
			build, err := p.Resolve(ctx, "")
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}

			start := time.Now()
			path, err := d.Fetch(ctx, build)
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			elapsed := time.Since(start)

			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat: %v", err)
			}
			if build.SizeBytes > 0 && info.Size() != build.SizeBytes {
				t.Fatalf("downloaded %d bytes, upstream declared %d", info.Size(), build.SizeBytes)
			}
			// A launcher is meant to be small; only a server jar can be
			// judged by its size.
			if build.IsLauncher() {
				t.Logf("%s %s: %.0f KB launcher — the server itself arrives on first start",
					build.Core, build.Version, float64(info.Size())/1024)
			} else if info.Size() < 1<<20 {
				t.Fatalf("downloaded only %d bytes — that is not a server jar", info.Size())
			}

			t.Logf("%s %s: %.1f MB verified in %s",
				build.Core, build.Version, float64(info.Size())/(1024*1024), elapsed.Round(time.Millisecond))

			// The second fetch must come from the cache and re-verify.
			if _, err := d.Fetch(ctx, build); err != nil {
				t.Fatalf("cached Fetch: %v", err)
			}
		})
	}
}

// TestLiveVanillaJavaMatchesTheTable compares the local Java table against
// what Mojang states per version. The table is a fallback used where upstream
// says nothing, so it drifting out of step is a real defect rather than
// cosmetic.
func TestLiveVanillaJavaMatchesTheTable(t *testing.T) {
	ctx := liveContext(t)

	v := NewVanilla(nil)
	versions, err := v.Versions(ctx)
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}

	// A handful of releases spanning every boundary the table encodes.
	probes := []string{"26.2", "1.21.11", "1.20.6", "1.20.4", "1.16.5"}

	for _, id := range probes {
		found := false
		for _, ver := range versions {
			if ver.ID == id {
				found = true
				break
			}
		}
		if !found {
			t.Logf("%s is no longer in the manifest, skipping", id)
			continue
		}

		build, err := v.Resolve(ctx, id)
		if err != nil {
			t.Errorf("Resolve(%s): %v", id, err)
			continue
		}

		// Mojang's value is authoritative; the table is what the panel falls
		// back on for cores that publish nothing.
		table := JavaMajorFor(id)
		if build.JavaMajor != table {
			// 1.17 is a known, deliberate disagreement.
			if strings.HasPrefix(id, "1.17") && table == Java17 {
				continue
			}
			t.Errorf("%s: Mojang says Java %d, the table says %d",
				id, build.JavaMajor, table)
		}
	}
}
