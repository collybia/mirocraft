package core

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// PufferfishCIBase is the Jenkins that publishes Pufferfish.
//
// Pufferfish has no version API: it is built on a CI, one job per Minecraft
// minor line, and the only thing a job reliably offers is its latest
// successful build. That shapes what this provider can honestly promise.
const PufferfishCIBase = "https://ci.pufferfish.host"

// pufferfishCacheTTL is how long the job list and their builds are reused.
// Reading them costs one request per job, and they change on the cadence of
// upstream releases rather than of panel page loads.
const pufferfishCacheTTL = 30 * time.Minute

// pufferfishJobPrefix marks the jobs this provider serves. The CI also builds
// Pufferfish-Purpur-*, which is a different fork and would be a different
// core; picking it up here would offer it under the wrong name.
const pufferfishJobPrefix = "Pufferfish-"

// pufferfishArtifact pulls the Minecraft version out of an artifact name such
// as pufferfish-paperclip-1.21.10-R0.1-SNAPSHOT-mojmap.jar.
var pufferfishArtifact = regexp.MustCompile(`pufferfish-paperclip-([0-9][^-]*)-`)

// Pufferfish serves Pufferfish builds from their CI.
//
// Only the current build of each line is offered. Walking Jenkins' build
// history to find an older patch version would be a request per build and
// would still miss the ones the CI has pruned; saying plainly that a version
// is not available beats a list whose entries fail when chosen.
type Pufferfish struct {
	// BaseURL is overridable for tests.
	BaseURL string
	Client  *http.Client

	mu       sync.Mutex
	cached   []pufferfishRelease
	cachedAt time.Time
	now      func() time.Time
}

// NewPufferfish returns the Pufferfish provider.
func NewPufferfish(client *http.Client) *Pufferfish {
	return &Pufferfish{BaseURL: PufferfishCIBase, Client: client, now: time.Now}
}

// ID returns the identifier the API and the database use for this core.
func (p *Pufferfish) ID() string { return "pufferfish" }

// Name returns the name shown in the panel.
func (p *Pufferfish) Name() string { return "Pufferfish" }

// Kind reports that this core is a server rather than a proxy.
func (p *Pufferfish) Kind() Kind { return KindServer }

// Runtime reports what has to be installed to run this core.
func (p *Pufferfish) Runtime() Runtime { return RuntimeJava }

// Content reports where this core's add-ons go.
//
// The loader is paper rather than pufferfish: Pufferfish is a Paper fork that
// the add-on registries do not list separately, and a plugin published for
// Paper is exactly what runs here. Inventing a loader nobody publishes for
// would make the catalogue return nothing.
func (p *Pufferfish) Content() Content {
	return Content{Loader: "paper", Dir: "plugins"}
}

// pufferfishRelease is one job's current build.
type pufferfishRelease struct {
	Job      string
	Build    int
	Version  string
	Artifact string
}

// jenkinsJobs is the CI's root document.
type jenkinsJobs struct {
	Jobs []struct {
		Name string `json:"name"`
	} `json:"jobs"`
}

// jenkinsBuild is one build.
type jenkinsBuild struct {
	Number    int    `json:"number"`
	Result    string `json:"result"`
	Artifacts []struct {
		RelativePath string `json:"relativePath"`
		FileName     string `json:"fileName"`
	} `json:"artifacts"`
}

// Versions lists what Pufferfish currently offers, newest first.
func (p *Pufferfish) Versions(ctx context.Context) ([]Version, error) {
	releases, err := p.releases(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]Version, 0, len(releases))
	for _, release := range releases {
		channel := ChannelSnapshot
		if IsRelease(release.Version) {
			channel = ChannelRelease
		}
		out = append(out, Version{
			ID:        release.Version,
			Channel:   channel,
			JavaMajor: JavaMajorFor(release.Version),
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		return CompareVersions(out[i].ID, out[j].ID) > 0
	})
	return out, nil
}

// Resolve returns the current build for a version.
func (p *Pufferfish) Resolve(ctx context.Context, version string) (*Build, error) {
	releases, err := p.releases(ctx)
	if err != nil {
		return nil, err
	}
	if len(releases) == 0 {
		return nil, fmt.Errorf("%w: pufferfish publishes no builds", ErrNoBuilds)
	}

	if version == "" {
		versions, err := p.Versions(ctx)
		if err != nil {
			return nil, err
		}
		version = newestRelease(versions)
	}

	for _, release := range releases {
		if release.Version != version {
			continue
		}
		return &Build{
			Core:     p.ID(),
			Version:  release.Version,
			Build:    strconv.Itoa(release.Build),
			URL:      fmt.Sprintf("%s/job/%s/%d/artifact/%s", p.base(), release.Job, release.Build, release.Artifact),
			FileName: fmt.Sprintf("pufferfish-%s-%d.jar", release.Version, release.Build),
			// Jenkins publishes no checksum with the artifact. Left empty
			// rather than invented; the download is over TLS and that is what
			// it can promise.
			JavaMajor: JavaMajorFor(release.Version),
		}, nil
	}

	return nil, fmt.Errorf("%w: pufferfish builds only the current version of each line, and %s is not one of them",
		ErrUnknownVersion, version)
}

// releases reads the CI, refreshing when stale.
func (p *Pufferfish) releases(ctx context.Context) ([]pufferfishRelease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cached != nil && p.now().Sub(p.cachedAt) < pufferfishCacheTTL {
		return p.cached, nil
	}

	var jobs jenkinsJobs
	if err := getJSON(ctx, p.Client, p.base()+"/api/json", &jobs); err != nil {
		return nil, fmt.Errorf("reading the pufferfish CI: %w", err)
	}

	var out []pufferfishRelease
	for _, job := range jobs.Jobs {
		if !strings.HasPrefix(job.Name, pufferfishJobPrefix) {
			continue
		}
		// Pufferfish-Purpur-* is a different fork; see the constant above.
		if strings.HasPrefix(job.Name, pufferfishJobPrefix+"Purpur") {
			continue
		}

		release, ok := p.readJob(ctx, job.Name)
		if !ok {
			// One job failing must not empty the list: an in-progress build
			// of one line should not hide the other five.
			continue
		}
		out = append(out, release)
	}

	p.cached, p.cachedAt = out, p.now()
	return out, nil
}

// readJob reads one job's latest successful build.
func (p *Pufferfish) readJob(ctx context.Context, job string) (pufferfishRelease, bool) {
	var build jenkinsBuild
	url := fmt.Sprintf("%s/job/%s/lastSuccessfulBuild/api/json", p.base(), job)
	if err := getJSON(ctx, p.Client, url, &build); err != nil {
		return pufferfishRelease{}, false
	}
	if build.Result != "" && build.Result != "SUCCESS" {
		return pufferfishRelease{}, false
	}

	for _, artifact := range build.Artifacts {
		match := pufferfishArtifact.FindStringSubmatch(artifact.FileName)
		if match == nil {
			continue
		}
		return pufferfishRelease{
			Job:      job,
			Build:    build.Number,
			Version:  match[1],
			Artifact: artifact.RelativePath,
		}, true
	}
	return pufferfishRelease{}, false
}

func (p *Pufferfish) base() string {
	if p.BaseURL == "" {
		return PufferfishCIBase
	}
	return p.BaseURL
}
