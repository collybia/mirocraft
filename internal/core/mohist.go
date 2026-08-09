package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

// MohistAPIBase is the MohistMC API.
const MohistAPIBase = "https://mohistmc.com/api/v2"

// mohistCacheTTL is how long the project's version list is reused.
const mohistCacheTTL = 30 * time.Minute

// Mohist serves Mohist, a hybrid that runs Forge mods and Bukkit plugins on
// the same server.
//
// The loader is paper: Mohist is a Bukkit-family server underneath, so what
// the catalogue should offer is Bukkit plugins. Its Forge mods come from
// elsewhere and are dropped into mods/ by hand, which the file manager covers.
type Mohist struct {
	// Project is the MohistMC project id. Mohist and Banner are published by
	// the same service in the same shape, so Banner is this type with a
	// different id rather than a copy of it.
	Project string
	// DisplayName is what the panel shows.
	DisplayName string

	// BaseURL is overridable for tests.
	BaseURL string
	Client  *http.Client

	mu       sync.Mutex
	cached   []string
	cachedAt time.Time
	now      func() time.Time
}

// NewMohist returns the Mohist provider.
func NewMohist(client *http.Client) *Mohist {
	return &Mohist{
		Project:     "mohist",
		DisplayName: "Mohist",
		BaseURL:     MohistAPIBase,
		Client:      client,
		now:         time.Now,
	}
}

// ID returns the identifier the API and the database use for this core.
func (m *Mohist) ID() string { return m.Project }

// Name returns the name shown in the panel.
func (m *Mohist) Name() string { return m.DisplayName }

// Kind reports that this core is a server rather than a proxy.
func (m *Mohist) Kind() Kind { return KindServer }

// Runtime reports what has to be installed to run this core.
func (m *Mohist) Runtime() Runtime { return RuntimeJava }

// Content reports where this core's add-ons go.
func (m *Mohist) Content() Content {
	return Content{Loader: "paper", Dir: "plugins"}
}

// mohistProject lists a project's Minecraft versions.
type mohistProject struct {
	Versions []string `json:"versions"`
}

// mohistBuilds lists the builds for one version.
type mohistBuilds struct {
	Builds []struct {
		ID         string `json:"id"`
		FileSHA256 string `json:"fileSha256"`
		URL        string `json:"url"`
		CreatedAt  int64  `json:"createdAt"`
	} `json:"builds"`
}

// Versions lists the Minecraft versions this project supports, newest first.
func (m *Mohist) Versions(ctx context.Context) ([]Version, error) {
	ids, err := m.versions(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]Version, 0, len(ids))
	for _, id := range ids {
		channel := ChannelSnapshot
		if IsRelease(id) {
			channel = ChannelRelease
		}
		out = append(out, Version{ID: id, Channel: channel, JavaMajor: JavaMajorFor(id)})
	}

	sort.SliceStable(out, func(i, j int) bool {
		return CompareVersions(out[i].ID, out[j].ID) > 0
	})
	return out, nil
}

// mohistLatestAttempts bounds how far back "latest" will look for a version
// that actually has a build.
const mohistLatestAttempts = 5

// Resolve picks the newest build for a version.
//
// An empty version means the newest one that can actually be installed, not
// the newest one listed: Mohist lists a Minecraft version as soon as work on
// it starts, and 1.21.4 sat in that list with no build behind it. Someone
// asking for the latest wants something that runs.
func (m *Mohist) Resolve(ctx context.Context, version string) (*Build, error) {
	if version != "" {
		return m.resolveOne(ctx, version)
	}

	versions, err := m.Versions(ctx)
	if err != nil {
		return nil, err
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("%w: %s publishes no versions", ErrUnknownVersion, m.Project)
	}

	var firstErr error
	attempts := 0
	for _, candidate := range versions {
		if candidate.Channel != ChannelRelease {
			continue
		}
		if attempts >= mohistLatestAttempts {
			break
		}
		attempts++

		build, err := m.resolveOne(ctx, candidate.ID)
		if err == nil {
			return build, nil
		}
		if firstErr == nil {
			firstErr = err
		}
		if !errors.Is(err, ErrNoBuilds) {
			// Something other than an empty version: retrying older versions
			// would just repeat it.
			return nil, err
		}
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return nil, fmt.Errorf("%w: %s publishes no installable version", ErrNoBuilds, m.Project)
}

// resolveOne resolves exactly the version asked for.
func (m *Mohist) resolveOne(ctx context.Context, version string) (*Build, error) {
	ids, err := m.versions(ctx)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("%w: %s publishes no versions", ErrUnknownVersion, m.Project)
	}

	known := false
	for _, id := range ids {
		if id == version {
			known = true
			break
		}
	}
	if !known {
		return nil, fmt.Errorf("%w: %s %s", ErrUnknownVersion, m.Project, version)
	}

	var builds mohistBuilds
	url := fmt.Sprintf("%s/projects/%s/%s/builds", m.base(), m.Project, version)
	if err := getJSON(ctx, m.Client, url, &builds); err != nil {
		return nil, fmt.Errorf("reading %s builds for %s: %w", m.Project, version, err)
	}
	if len(builds.Builds) == 0 {
		// A version can be listed before anything is built for it, which is
		// what 1.21.4 looked like when this was written. Said plainly, because
		// the alternative reading — "the panel is broken" — sends someone to
		// the wrong place.
		return nil, fmt.Errorf("%w: %s lists %s but has published no build for it yet",
			ErrNoBuilds, m.Project, version)
	}

	// Newest last in their listing; the timestamp decides rather than the
	// position, because a listing order is not a promise.
	newest := builds.Builds[0]
	for _, build := range builds.Builds[1:] {
		if build.CreatedAt > newest.CreatedAt {
			newest = build
		}
	}
	if newest.URL == "" {
		return nil, fmt.Errorf("%w: %s %s publishes no download", ErrNoBuilds, m.Project, version)
	}

	return &Build{
		Core:      m.ID(),
		Version:   version,
		Build:     newest.ID,
		URL:       newest.URL,
		FileName:  fmt.Sprintf("%s-%s.jar", m.Project, version),
		Checksum:  newest.FileSHA256,
		Algorithm: AlgoSHA256,
		JavaMajor: JavaMajorFor(version),
	}, nil
}

// versions reads the project's version list, refreshing when stale.
func (m *Mohist) versions(ctx context.Context) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.cached != nil && m.now().Sub(m.cachedAt) < mohistCacheTTL {
		return m.cached, nil
	}

	var project mohistProject
	url := fmt.Sprintf("%s/projects/%s", m.base(), m.Project)
	if err := getJSON(ctx, m.Client, url, &project); err != nil {
		return nil, fmt.Errorf("reading the %s project: %w", m.Project, err)
	}

	m.cached, m.cachedAt = project.Versions, m.now()
	return m.cached, nil
}

func (m *Mohist) base() string {
	if m.BaseURL == "" {
		return MohistAPIBase
	}
	return m.BaseURL
}
