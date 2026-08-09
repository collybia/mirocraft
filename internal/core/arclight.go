package core

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"sync"
	"time"
)

// ArclightReleasesURL is where Arclight publishes its builds.
//
// GitHub rather than a project API because Arclight has none. That has a cost:
// unauthenticated GitHub allows sixty requests an hour per address, shared
// with everything else on the machine, which is why this is cached hard.
const ArclightReleasesURL = "https://api.github.com/repos/IzzelAliz/Arclight/releases?per_page=30"

// arclightCacheTTL is how long the release list is reused. Half an hour puts
// this at two requests an hour against GitHub's sixty.
const arclightCacheTTL = 30 * time.Minute

// arclightAsset picks the Minecraft version and loader out of an asset name
// such as arclight-forge-1.21.1-1.0.1-8ec9529.jar.
var arclightAsset = regexp.MustCompile(`^arclight-(forge|neoforge|fabric)-([0-9][^-]*)-(.+)\.jar$`)

// arclightPreferred is the mod loader chosen when a release publishes several.
//
// Forge first because Arclight exists to run Forge mods alongside Bukkit
// plugins, and someone picking it is far more likely to have a Forge modpack
// than a Fabric one. The others are still installable by asking for the
// version they are the only build for.
var arclightPreferred = []string{"forge", "neoforge", "fabric"}

// Arclight serves Arclight, a hybrid running Forge or Fabric mods alongside
// Bukkit plugins.
type Arclight struct {
	// ReleasesURL is overridable for tests.
	ReleasesURL string
	Client      *http.Client

	mu       sync.Mutex
	cached   map[string]arclightBuild // minecraft version -> chosen asset
	cachedAt time.Time
	now      func() time.Time
}

// arclightBuild is one downloadable asset.
type arclightBuild struct {
	Minecraft string
	Loader    string
	Build     string
	Name      string
	URL       string
	Size      int64
}

// NewArclight returns the Arclight provider.
func NewArclight(client *http.Client) *Arclight {
	return &Arclight{ReleasesURL: ArclightReleasesURL, Client: client, now: time.Now}
}

// ID returns the identifier the API and the database use for this core.
func (a *Arclight) ID() string { return "arclight" }

// Name returns the name shown in the panel.
func (a *Arclight) Name() string { return "Arclight" }

// Kind reports that this core is a server rather than a proxy.
func (a *Arclight) Kind() Kind { return KindServer }

// Runtime reports what has to be installed to run this core.
func (a *Arclight) Runtime() Runtime { return RuntimeJava }

// Content reports where this core's add-ons go. Bukkit plugins, like Mohist:
// the mods it also runs come from elsewhere and go into mods/ by hand.
func (a *Arclight) Content() Content {
	return Content{Loader: "paper", Dir: "plugins"}
}

// githubRelease is one release from the GitHub API.
type githubRelease struct {
	TagName    string `json:"tag_name"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

// Versions lists the Minecraft versions Arclight publishes a build for.
func (a *Arclight) Versions(ctx context.Context) ([]Version, error) {
	builds, err := a.builds(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]Version, 0, len(builds))
	for minecraft := range builds {
		channel := ChannelSnapshot
		if IsRelease(minecraft) {
			channel = ChannelRelease
		}
		out = append(out, Version{
			ID:        minecraft,
			Channel:   channel,
			JavaMajor: JavaMajorFor(minecraft),
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		return CompareVersions(out[i].ID, out[j].ID) > 0
	})
	return out, nil
}

// Resolve returns the newest Arclight build for a Minecraft version.
func (a *Arclight) Resolve(ctx context.Context, version string) (*Build, error) {
	builds, err := a.builds(ctx)
	if err != nil {
		return nil, err
	}
	if len(builds) == 0 {
		return nil, fmt.Errorf("%w: arclight publishes no builds", ErrNoBuilds)
	}

	if version == "" {
		versions, err := a.Versions(ctx)
		if err != nil {
			return nil, err
		}
		version = newestRelease(versions)
	}

	chosen, ok := builds[version]
	if !ok {
		return nil, fmt.Errorf("%w: arclight %s", ErrUnknownVersion, version)
	}

	return &Build{
		Core:     a.ID(),
		Version:  version,
		Build:    chosen.Build,
		URL:      chosen.URL,
		FileName: chosen.Name,
		// GitHub publishes a digest per asset in newer API versions but not
		// in the release listing, so nothing is claimed here.
		SizeBytes: chosen.Size,
		JavaMajor: JavaMajorFor(version),
	}, nil
}

// builds reads the releases, refreshing when stale.
func (a *Arclight) builds(ctx context.Context) (map[string]arclightBuild, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.cached != nil && a.now().Sub(a.cachedAt) < arclightCacheTTL {
		return a.cached, nil
	}

	var releases []githubRelease
	if err := getJSON(ctx, a.Client, a.releases(), &releases); err != nil {
		return nil, fmt.Errorf("reading arclight releases: %w", err)
	}

	// Releases come newest first, so the first asset seen for a Minecraft
	// version is the newest build for it.
	out := make(map[string]arclightBuild, len(releases))
	for _, release := range releases {
		if release.Prerelease {
			continue
		}
		for _, asset := range release.Assets {
			match := arclightAsset.FindStringSubmatch(asset.Name)
			if match == nil {
				continue
			}
			loader, minecraft, build := match[1], match[2], match[3]

			existing, seen := out[minecraft]
			if seen && preferredRank(existing.Loader) <= preferredRank(loader) {
				// Either the same release already gave a loader we prefer, or
				// an older release did — and an older release never wins,
				// because the listing is newest first.
				continue
			}
			out[minecraft] = arclightBuild{
				Minecraft: minecraft, Loader: loader, Build: build,
				Name: asset.Name, URL: asset.URL, Size: asset.Size,
			}
		}
	}

	a.cached, a.cachedAt = out, a.now()
	return a.cached, nil
}

// preferredRank orders the loaders, lower being preferred.
func preferredRank(loader string) int {
	for i, candidate := range arclightPreferred {
		if candidate == loader {
			return i
		}
	}
	return len(arclightPreferred)
}

func (a *Arclight) releases() string {
	if a.ReleasesURL == "" {
		return ArclightReleasesURL
	}
	return a.ReleasesURL
}
