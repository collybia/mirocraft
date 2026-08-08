package catalog

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPlanInstallPicksTheNewestRelease(t *testing.T) {
	client := newFakeUpstream(t).client()

	plan, err := client.PlanInstall(context.Background(), "needs-lib", "", "paper", "1.21.4")
	if err != nil {
		t.Fatalf("planning: %v", err)
	}

	// The alpha is newer, and Modrinth returns it first. Someone clicking
	// "install" means the release; a pre-release is a deliberate choice, made
	// by naming a version.
	if plan.Files[0].VersionName != "2.0.0" {
		t.Fatalf("chose version %q, want the release rather than the newer alpha",
			plan.Files[0].VersionName)
	}
}

func TestPlanInstallFollowsRequiredDependencies(t *testing.T) {
	client := newFakeUpstream(t).client()

	plan, err := client.PlanInstall(context.Background(), "needs-lib", "", "paper", "1.21.4")
	if err != nil {
		t.Fatalf("planning: %v", err)
	}

	if len(plan.Files) != 2 {
		t.Fatalf("planned %d files, want the project and its library: %+v", len(plan.Files), plan.Files)
	}

	// The requested project first, so the panel can show what was asked for
	// above what came with it.
	if plan.Files[0].Dependency {
		t.Error("the requested project is marked as a dependency")
	}
	if !plan.Files[1].Dependency {
		t.Error("the library is not marked as a dependency")
	}
	if plan.Files[1].FileName != "the-lib-1.4.0.jar" {
		t.Errorf("dependency file = %q", plan.Files[1].FileName)
	}

	// Optional means "works better with", and embedded means the jar already
	// contains it. Installing either uninvited gives an operator plugins they
	// never chose, or two copies of the same classes.
	for _, file := range plan.Files {
		if file.ProjectID == "nice-to-have" || file.ProjectID == "bundled" {
			t.Errorf("an %s dependency was installed: %s", file.ProjectID, file.FileName)
		}
	}
}

func TestPlanInstallReportsTotalSize(t *testing.T) {
	client := newFakeUpstream(t).client()

	plan, err := client.PlanInstall(context.Background(), "needs-lib", "", "paper", "1.21.4")
	if err != nil {
		t.Fatalf("planning: %v", err)
	}
	if got := plan.TotalBytes(); got != 20+30 {
		t.Fatalf("total = %d, want the sum of both files", got)
	}
}

func TestPlanInstallByExplicitVersion(t *testing.T) {
	client := newFakeUpstream(t).client()

	plan, err := client.PlanInstall(context.Background(), "essentialsx", "SKQwLLoQ", "paper", "1.21.4")
	if err != nil {
		t.Fatalf("planning: %v", err)
	}
	if len(plan.Files) != 1 || plan.Files[0].VersionID != "SKQwLLoQ" {
		t.Fatalf("plan = %+v", plan.Files)
	}
	if plan.Files[0].SHA512 != "ddd" {
		t.Errorf("the plan carries no checksum: %+v", plan.Files[0])
	}
}

// Nothing published for this loader is the common case for someone browsing
// a plugin on the wrong kind of server, and it deserves its own error.
func TestPlanInstallWithNothingPublished(t *testing.T) {
	client := newFakeUpstream(t).client()

	_, err := client.PlanInstall(context.Background(), "unknown-project", "", "fabric", "1.21.4")
	if err == nil {
		t.Fatal("planning an impossible install succeeded")
	}
	if !errors.Is(err, ErrNoVersions) && !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want a distinguishable one", err)
	}
}

func TestPickNewestPrefersReleases(t *testing.T) {
	newest := pickNewest([]Version{
		{ID: "alpha", Channel: "alpha", PublishedAt: parseTime(t, "2026-03-01T00:00:00Z")},
		{ID: "beta", Channel: "beta", PublishedAt: parseTime(t, "2026-02-01T00:00:00Z")},
		{ID: "old-release", Channel: "release", PublishedAt: parseTime(t, "2026-01-01T00:00:00Z")},
		{ID: "new-release", Channel: "release", PublishedAt: parseTime(t, "2026-01-15T00:00:00Z")},
	})

	if newest.ID != "new-release" {
		t.Fatalf("picked %q, want the newest release", newest.ID)
	}
}

// With nothing stable published, a beta beats nothing at all.
func TestPickNewestFallsBackToPreReleases(t *testing.T) {
	newest := pickNewest([]Version{
		{ID: "alpha", Channel: "alpha", PublishedAt: parseTime(t, "2026-03-01T00:00:00Z")},
		{ID: "beta", Channel: "beta", PublishedAt: parseTime(t, "2026-02-01T00:00:00Z")},
	})
	if newest.ID != "beta" {
		t.Fatalf("picked %q, want the beta over the alpha", newest.ID)
	}
}

func parseTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatalf("parsing %q: %v", value, err)
	}
	return parsed
}
