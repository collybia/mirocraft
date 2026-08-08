package catalog

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// These run against the real Modrinth API.
//
// Worth having despite the network dependency: every shape this package parses
// was learned by asking the live service, and a client tested only against its
// own fixtures verifies the fixtures. Upstream changing a field name is
// precisely the failure these catch and unit tests cannot.
//
// Skipped by default so the suite stays offline-clean; set MIROCRAFT_LIVE=1 in
// CI, or run them by hand when touching this package.
func requireLive(t *testing.T) *Client {
	t.Helper()

	if os.Getenv("MIROCRAFT_LIVE") == "" {
		t.Skip("set MIROCRAFT_LIVE=1 to run against the real Modrinth API")
	}
	return New(nil)
}

func TestLiveSearchFindsAKnownPlugin(t *testing.T) {
	client := requireLive(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	result, err := client.Search(ctx, SearchOptions{
		Query: "EssentialsX", Loader: "paper", GameVersion: "1.21.4", Limit: 10,
	})
	if err != nil {
		t.Fatalf("searching: %v", err)
	}
	if len(result.Items) == 0 {
		t.Fatal("no hits for a plugin that certainly exists")
	}

	var found *Project
	for i := range result.Items {
		if result.Items[i].Slug == "essentialsx" {
			found = &result.Items[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("essentialsx was not among %d hits", len(result.Items))
	}
	if found.Title == "" || found.Downloads == 0 {
		t.Errorf("the hit is missing fields the panel shows: %+v", found)
	}
	// The filter that keeps client-only mods off a server list.
	if found.ServerSide == "unsupported" {
		t.Errorf("server_side = %q slipped through the facet", found.ServerSide)
	}
}

func TestLivePlanInstallResolvesAFile(t *testing.T) {
	client := requireLive(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	plan, err := client.PlanInstall(ctx, "essentialsx", "", "paper", "1.21.4")
	if err != nil {
		t.Fatalf("planning: %v", err)
	}
	if len(plan.Files) == 0 {
		t.Fatal("the plan has no files")
	}

	file := plan.Files[0]
	if file.URL == "" || file.SizeBytes == 0 {
		t.Fatalf("the planned file is unusable: %+v", file)
	}
	// Without a checksum the install cannot verify what it downloaded, so it
	// is worth knowing if upstream ever stops publishing one.
	if file.SHA512 == "" && file.SHA1 == "" {
		t.Errorf("upstream published no checksum for %s", file.FileName)
	}
	t.Logf("plan: %s %s (%d bytes), %d file(s) total",
		file.ProjectTitle, file.VersionName, file.SizeBytes, len(plan.Files))
}

func TestLiveUnknownProjectIsNotFound(t *testing.T) {
	client := requireLive(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := client.Project(ctx, "mirocraft-definitely-not-a-project-xyz")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}
