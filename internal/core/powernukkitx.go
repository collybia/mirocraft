package core

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// PowerNukkitXReleasesURL is where PowerNukkitX publishes its builds.
const PowerNukkitXReleasesURL = "https://api.github.com/repos/PowerNukkitX/PowerNukkitX/releases?per_page=20"

// powerNukkitXCacheTTL keeps this to two GitHub requests an hour against
// their sixty per address.
const powerNukkitXCacheTTL = 30 * time.Minute

// powerNukkitXAsset is the file to run. Their releases also carry a launcher
// script and checksums, and picking by extension alone would install one.
const powerNukkitXAsset = "powernukkitx.jar"

// PowerNukkitX serves PowerNukkitX, a Bedrock server written in Java.
//
// A Bedrock server that runs on the JVM: the protocol its players speak is
// Bedrock, so it is KindBedrock and listens on UDP, while what starts it is an
// ordinary jar. The two questions are separate, which is why Kind and Runtime
// are separate.
type PowerNukkitX struct {
	// ReleasesURL is overridable for tests.
	ReleasesURL string
	Client      *http.Client

	mu       sync.Mutex
	cached   []powerNukkitXBuild
	cachedAt time.Time
	now      func() time.Time
}

type powerNukkitXBuild struct {
	Version string
	URL     string
	Size    int64
}

// NewPowerNukkitX returns the PowerNukkitX provider.
func NewPowerNukkitX(client *http.Client) *PowerNukkitX {
	return &PowerNukkitX{ReleasesURL: PowerNukkitXReleasesURL, Client: client, now: time.Now}
}

// ID returns the identifier the API and the database use for this core.
func (p *PowerNukkitX) ID() string { return "powernukkitx" }

// Name returns the name shown in the panel.
func (p *PowerNukkitX) Name() string { return "PowerNukkitX" }

// Kind reports that players connect with a Bedrock client.
func (p *PowerNukkitX) Kind() Kind { return KindBedrock }

// Runtime reports that it runs on the JVM, unlike Mojang's own server.
func (p *PowerNukkitX) Runtime() Runtime { return RuntimeJava }

// Content reports where its plugins go. Nukkit plugins are their own thing:
// a Bukkit jar does not run here.
func (p *PowerNukkitX) Content() Content {
	return Content{Loader: "nukkit", Dir: "plugins"}
}

// Versions lists the releases, newest first.
//
// Its own version rather than a Minecraft one: PowerNukkitX names releases
// after itself and states which Bedrock protocol they speak in the notes, so
// a version list keyed by Minecraft version would be an invention.
func (p *PowerNukkitX) Versions(ctx context.Context) ([]Version, error) {
	builds, err := p.builds(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]Version, 0, len(builds))
	for _, build := range builds {
		out = append(out, Version{
			ID:      build.Version,
			Channel: ChannelRelease,
			// Their own requirement, stated in the project's readme rather
			// than derivable from a Minecraft version they do not name.
			JavaMajor: Java21,
		})
	}
	return out, nil
}

// Resolve returns a release's jar.
func (p *PowerNukkitX) Resolve(ctx context.Context, version string) (*Build, error) {
	builds, err := p.builds(ctx)
	if err != nil {
		return nil, err
	}
	if len(builds) == 0 {
		return nil, fmt.Errorf("%w: powernukkitx publishes no builds", ErrNoBuilds)
	}

	if version == "" {
		version = builds[0].Version
	}

	for _, build := range builds {
		if build.Version != version {
			continue
		}
		return &Build{
			Core:     p.ID(),
			Version:  build.Version,
			URL:      build.URL,
			FileName: fmt.Sprintf("powernukkitx-%s.jar", build.Version),
			// GitHub states no digest in the release listing.
			SizeBytes: build.Size,
			JavaMajor: Java21,
		}, nil
	}
	return nil, fmt.Errorf("%w: powernukkitx %s", ErrUnknownVersion, version)
}

// builds reads the releases, refreshing when stale.
func (p *PowerNukkitX) builds(ctx context.Context) ([]powerNukkitXBuild, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cached != nil && p.now().Sub(p.cachedAt) < powerNukkitXCacheTTL {
		return p.cached, nil
	}

	var releases []githubRelease
	if err := getJSON(ctx, p.Client, p.releases(), &releases); err != nil {
		return nil, fmt.Errorf("reading powernukkitx releases: %w", err)
	}

	var out []powerNukkitXBuild
	for _, release := range releases {
		if release.Prerelease {
			continue
		}
		for _, asset := range release.Assets {
			if asset.Name != powerNukkitXAsset {
				continue
			}
			out = append(out, powerNukkitXBuild{
				Version: release.TagName, URL: asset.URL, Size: asset.Size,
			})
			break
		}
	}

	p.cached, p.cachedAt = out, p.now()
	return p.cached, nil
}

func (p *PowerNukkitX) releases() string {
	if p.ReleasesURL == "" {
		return PowerNukkitXReleasesURL
	}
	return p.ReleasesURL
}
