package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

// PaperAPIBase is the PaperMC API root.
//
// This is "fill", the service that replaced the old api.papermc.io/v2. That
// one now answers 410 with {"error":"sunset"}, so anything still pointing at
// it is broken rather than merely outdated.
const PaperAPIBase = "https://fill.papermc.io/v3"

// downloadKey is the artifact a server needs. The API returns a map keyed by
// artifact name, and Paper also publishes mojang-mapped jars under a
// different key that is not what an operator wants to run.
const downloadKey = "server:default"

// Paper serves PaperMC builds. The same API covers Folia, Velocity and
// Waterfall, so the project is a field rather than a constant — task 6.x adds
// those by constructing this type with a different project id.
type Paper struct {
	// Project is the PaperMC project id: paper, folia, velocity, waterfall.
	Project string
	// DisplayName is what the panel shows.
	DisplayName string
	// ServerKind distinguishes proxies from servers.
	ServerKind Kind

	// BaseURL is overridable for tests.
	BaseURL string
	Client  *http.Client

	mu       sync.Mutex
	cached   *paperProject
	cachedAt time.Time
	now      func() time.Time
}

// NewPaper returns the Paper provider.
func NewPaper(client *http.Client) *Paper {
	return NewPaperProject("paper", "Paper", KindServer, client)
}

// NewPaperProject returns a provider for any project on the PaperMC API.
//
// Folia, Velocity and Waterfall are published by the same service in the same
// shape, so they are this type with a different id rather than three copies of
// it. What differs between them — the name, and whether it is a proxy — is
// exactly what this takes as arguments.
func NewPaperProject(id, name string, kind Kind, client *http.Client) *Paper {
	return &Paper{
		Project:     id,
		DisplayName: name,
		ServerKind:  kind,
		BaseURL:     PaperAPIBase,
		Client:      client,
		now:         time.Now,
	}
}

// NewFolia returns the Folia provider.
//
// Folia is Paper with regionised threading, and it takes Paper's plugins only
// when they are written for it — but the add-on registries list folia as its
// own loader, so a plugin that declares Folia support is what the catalogue
// offers here.
func NewFolia(client *http.Client) *Paper {
	return NewPaperProject("folia", "Folia", KindServer, client)
}

// ID returns the identifier the API and the database use for this core.
func (p *Paper) ID() string { return p.Project }

// Name returns the name shown in the panel.
func (p *Paper) Name() string { return p.DisplayName }

// Kind reports whether this core is a server, a proxy or a Bedrock server.
func (p *Paper) Kind() Kind {
	if p.ServerKind == "" {
		return KindServer
	}
	return p.ServerKind
}

// Runtime reports what has to be installed to run this core.
func (p *Paper) Runtime() Runtime { return RuntimeJava }

// Content reports where this project's add-ons go.
//
// A proxy takes plugins too, but its own: a Velocity plugin and a Paper plugin
// are different things that happen to share a folder name, which is why the
// loader is the project id rather than a constant.
func (p *Paper) Content() Content {
	return Content{Loader: p.Project, Dir: "plugins"}
}

// paperProject is the project document. Versions are grouped by family:
// {"26.2": ["26.2", "26.2-rc-2"], "1.21": ["1.21.11", ...]}.
type paperProject struct {
	Project struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"project"`
	Versions map[string][]string `json:"versions"`
}

// flatten returns every version id the project supports.
func (p *paperProject) flatten() []string {
	var out []string
	for _, family := range p.Versions {
		out = append(out, family...)
	}
	return out
}

// paperBuild is a single build.
type paperBuild struct {
	ID        int                      `json:"id"`
	Time      time.Time                `json:"time"`
	Channel   string                   `json:"channel"`
	Downloads map[string]paperArtifact `json:"downloads"`
}

type paperArtifact struct {
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	URL       string `json:"url"`
	Checksums struct {
		SHA256 string `json:"sha256"`
	} `json:"checksums"`
}

// Versions lists the Minecraft versions this project supports, newest first.
func (p *Paper) Versions(ctx context.Context) ([]Version, error) {
	project, err := p.project(ctx)
	if err != nil {
		return nil, err
	}

	ids := project.flatten()
	out := make([]Version, 0, len(ids))
	for _, id := range ids {
		channel := ChannelSnapshot
		if IsRelease(id) {
			channel = ChannelRelease
		}
		out = append(out, Version{
			ID:        id,
			Channel:   channel,
			JavaMajor: JavaMajorFor(id),
		})
	}

	// The API groups versions in a JSON object, and Go map iteration is
	// deliberately random, so ordering here is not cosmetic — without it the
	// version list would come out shuffled on every request.
	sort.SliceStable(out, func(i, j int) bool {
		return CompareVersions(out[i].ID, out[j].ID) > 0
	})
	return out, nil
}

// Resolve picks the latest build for a version. An empty version means the
// newest release the project supports — never a release candidate, which
// sorts high but is not what somebody asking for "latest" wants to run.
func (p *Paper) Resolve(ctx context.Context, version string) (*Build, error) {
	project, err := p.project(ctx)
	if err != nil {
		return nil, err
	}

	ids := project.flatten()
	if len(ids) == 0 {
		return nil, fmt.Errorf("%w: %s publishes no versions", ErrUnknownVersion, p.Project)
	}

	if version == "" {
		versions, err := p.Versions(ctx)
		if err != nil {
			return nil, err
		}
		for _, v := range versions {
			if v.Channel == ChannelRelease {
				version = v.ID
				break
			}
		}
		if version == "" {
			// Nothing but pre-releases: better the newest of those than an
			// error, since the project genuinely has nothing else.
			version = versions[0].ID
		}
	}

	known := false
	for _, id := range ids {
		if id == version {
			known = true
			break
		}
	}
	if !known {
		return nil, fmt.Errorf("%w: %s %s", ErrUnknownVersion, p.Project, version)
	}

	var build paperBuild
	url := fmt.Sprintf("%s/projects/%s/versions/%s/builds/latest", p.base(), p.Project, version)
	if err := getJSON(ctx, p.Client, url, &build); err != nil {
		if errors.Is(err, ErrUnknownVersion) {
			return nil, fmt.Errorf("%w: %s %s", ErrNoBuilds, p.Project, version)
		}
		return nil, fmt.Errorf("reading the latest build of %s %s: %w", p.Project, version, err)
	}

	artifact, ok := build.Downloads[downloadKey]
	if !ok || artifact.URL == "" {
		return nil, fmt.Errorf("build %d of %s %s publishes no %s download",
			build.ID, p.Project, version, downloadKey)
	}

	return &Build{
		Core:      p.ID(),
		Version:   version,
		Build:     strconv.Itoa(build.ID),
		URL:       artifact.URL,
		FileName:  artifact.Name,
		Checksum:  artifact.Checksums.SHA256,
		Algorithm: AlgoSHA256,
		SizeBytes: artifact.Size,
		JavaMajor: JavaMajorFor(version),
	}, nil
}

// project returns the project document, refreshing it when stale.
func (p *Paper) project(ctx context.Context) (*paperProject, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now
	if p.now != nil {
		now = p.now
	}
	if p.cached != nil && now().Sub(p.cachedAt) < manifestTTL {
		return p.cached, nil
	}

	var project paperProject
	url := fmt.Sprintf("%s/projects/%s", p.base(), p.Project)
	if err := getJSON(ctx, p.Client, url, &project); err != nil {
		// As with vanilla, a stale list beats none when upstream is briefly
		// unreachable.
		if p.cached != nil {
			return p.cached, nil
		}
		return nil, fmt.Errorf("reading the %s project: %w", p.Project, err)
	}

	p.cached = &project
	p.cachedAt = now()
	return p.cached, nil
}

func (p *Paper) base() string {
	if p.BaseURL == "" {
		return PaperAPIBase
	}
	return p.BaseURL
}

// DefaultRegistry returns the providers implemented so far. Task 6.x extends
// this with the remaining cores.
func DefaultRegistry(client *http.Client) *Registry {
	r := NewRegistry()
	// Registration order is the order the panel shows them in, so the ones
	// most people want come first.
	r.Register(NewVanilla(client))
	r.Register(NewPaper(client))
	r.Register(NewPurpur(client))
	r.Register(NewPufferfish(client))
	r.Register(NewFolia(client))
	r.Register(NewFabric(client))
	r.Register(NewQuilt(client))
	r.Register(NewForge(client))
	r.Register(NewNeoForge(client))
	return r
}
