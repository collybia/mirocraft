package catalog

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// MaxDependencyDepth bounds how far a dependency chain is followed.
//
// A plugin needing a library that needs another is normal; ten levels is a
// cycle Modrinth's data did not declare, or a project pulling in half the
// registry. Either way an operator who clicked "install" should not get a
// hundred jars.
const MaxDependencyDepth = 5

// Plan is what installing a project will actually do.
//
// Produced before anything is downloaded so the panel can show it and the
// operator can see that one click brings four files.
type Plan struct {
	// Files are the artifacts to download, the requested one first.
	Files []PlannedFile `json:"files"`
	// Skipped names dependencies that could not be resolved, with the reason.
	// Reported rather than swallowed: a plugin missing a required library
	// fails at server start with a stack trace, and knowing now is cheaper.
	Skipped []SkippedDependency `json:"skipped,omitempty"`
}

// PlannedFile is one artifact and where it came from.
type PlannedFile struct {
	ProjectID    string `json:"project_id"`
	ProjectTitle string `json:"project_title"`
	VersionID    string `json:"version_id"`
	VersionName  string `json:"version_name"`
	FileName     string `json:"file_name"`
	URL          string `json:"url"`
	SizeBytes    int64  `json:"size_bytes"`
	SHA512       string `json:"sha512,omitempty"`
	SHA1         string `json:"sha1,omitempty"`
	// Dependency marks a file pulled in by another rather than asked for.
	Dependency bool `json:"dependency"`
}

// SkippedDependency is a requirement that could not be met.
type SkippedDependency struct {
	ProjectID string `json:"project_id"`
	Reason    string `json:"reason"`
}

// TotalBytes is the size of everything the plan will download.
func (p *Plan) TotalBytes() int64 {
	var total int64
	for _, file := range p.Files {
		total += file.SizeBytes
	}
	return total
}

// PlanInstall resolves a project into the files to download.
//
// versionID may be empty, in which case the newest version matching the loader
// and Minecraft version is chosen — which is what "install" means to someone
// who has not gone looking for a particular release.
func (c *Client) PlanInstall(ctx context.Context, projectID, versionID, loader, gameVersion string) (*Plan, error) {
	root, err := c.resolveVersion(ctx, projectID, versionID, loader, gameVersion)
	if err != nil {
		return nil, err
	}

	plan := &Plan{}
	seen := map[string]bool{}

	if err := c.addVersion(ctx, plan, seen, root, loader, gameVersion, false, 0); err != nil {
		return nil, err
	}
	if len(plan.Files) == 0 {
		return nil, fmt.Errorf("%w: the version has no downloadable file", ErrNoVersions)
	}
	return plan, nil
}

// resolveVersion picks the version to install.
func (c *Client) resolveVersion(ctx context.Context, projectID, versionID, loader, gameVersion string) (*Version, error) {
	if versionID != "" {
		return c.VersionByID(ctx, versionID)
	}

	versions, err := c.Versions(ctx, projectID, loader, gameVersion)
	if err != nil {
		return nil, err
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("%w: %s for %s %s", ErrNoVersions, projectID, loader, gameVersion)
	}
	return pickNewest(versions), nil
}

// pickNewest prefers a stable release over a beta or alpha of the same age.
//
// Modrinth returns versions newest first, so taking the first would hand an
// operator an alpha whenever the author published one yesterday. A release is
// what someone clicking "install" expects; a pre-release is a deliberate
// choice, made by picking a version explicitly.
func pickNewest(versions []Version) *Version {
	sorted := make([]Version, len(versions))
	copy(sorted, versions)

	rank := func(channel string) int {
		switch channel {
		case "release":
			return 0
		case "beta":
			return 1
		default:
			return 2
		}
	}
	sort.SliceStable(sorted, func(i, j int) bool {
		if rank(sorted[i].Channel) != rank(sorted[j].Channel) {
			return rank(sorted[i].Channel) < rank(sorted[j].Channel)
		}
		return sorted[i].PublishedAt.After(sorted[j].PublishedAt)
	})
	return &sorted[0]
}

// addVersion appends a version's file and follows its required dependencies.
func (c *Client) addVersion(
	ctx context.Context,
	plan *Plan,
	seen map[string]bool,
	version *Version,
	loader, gameVersion string,
	isDependency bool,
	depth int,
) error {
	if seen[version.ProjectID] {
		return nil
	}
	seen[version.ProjectID] = true

	file, ok := version.PrimaryFile()
	if !ok {
		plan.Skipped = append(plan.Skipped, SkippedDependency{
			ProjectID: version.ProjectID, Reason: "the version publishes no file",
		})
		return nil
	}

	title := version.ProjectID
	if project, err := c.Project(ctx, version.ProjectID); err == nil {
		title = project.Title
	}

	plan.Files = append(plan.Files, PlannedFile{
		ProjectID: version.ProjectID, ProjectTitle: title,
		VersionID: version.ID, VersionName: version.Number,
		FileName: file.Name, URL: file.URL, SizeBytes: file.Size,
		SHA512: file.SHA512, SHA1: file.SHA1,
		Dependency: isDependency,
	})

	if depth >= MaxDependencyDepth {
		return nil
	}

	for _, dep := range version.Dependencies {
		// Only required ones. Optional dependencies are the author saying
		// "this works better with", and installing them uninvited is how an
		// operator ends up with plugins they never chose.
		//
		// Embedded means the jar already contains it; installing it again
		// gives two copies of the same classes.
		if dep.Type != "required" {
			continue
		}
		if dep.ProjectID != "" && seen[dep.ProjectID] {
			continue
		}

		resolved, err := c.resolveDependency(ctx, dep, loader, gameVersion)
		if err != nil {
			plan.Skipped = append(plan.Skipped, SkippedDependency{
				ProjectID: dependencyName(dep), Reason: err.Error(),
			})
			continue
		}
		if err := c.addVersion(ctx, plan, seen, resolved, loader, gameVersion, true, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) resolveDependency(ctx context.Context, dep Dependency, loader, gameVersion string) (*Version, error) {
	if dep.VersionID != "" {
		return c.VersionByID(ctx, dep.VersionID)
	}
	if dep.ProjectID == "" {
		return nil, errors.New("the dependency names neither a project nor a version")
	}
	return c.resolveVersion(ctx, dep.ProjectID, "", loader, gameVersion)
}

func dependencyName(dep Dependency) string {
	if dep.ProjectID != "" {
		return dep.ProjectID
	}
	return dep.VersionID
}
