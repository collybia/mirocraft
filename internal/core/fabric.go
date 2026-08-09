package core

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// FabricMetaBase is the Fabric metadata API root.
const FabricMetaBase = "https://meta.fabricmc.net/v2"

// fabricCacheTTL is how long the loader and installer lists are reused.
//
// They change a few times a month, and every Resolve needs both: without a
// cache, creating ten servers would make thirty requests to answer a question
// with the same answer each time.
const fabricCacheTTL = time.Hour

// Fabric serves the Fabric mod loader.
//
// Fabric publishes a ready-to-run server launcher for any combination of
// game, loader and installer version, so nothing has to be executed at install
// time. What arrives is not the server, though: it is a small jar that fetches
// the Minecraft server and the loader's libraries the first time it runs, so
// the first start of a Fabric server needs the network.
type Fabric struct {
	// BaseURL is overridable for tests.
	BaseURL string
	Client  *http.Client

	mu        sync.Mutex
	loader    string
	installer string
	cachedAt  time.Time
	now       func() time.Time
}

// NewFabric returns the Fabric provider.
func NewFabric(client *http.Client) *Fabric {
	return &Fabric{BaseURL: FabricMetaBase, Client: client, now: time.Now}
}

// ID returns the identifier the API and the database use for this core.
func (f *Fabric) ID() string { return "fabric" }

// Name returns the name shown in the panel.
func (f *Fabric) Name() string { return "Fabric" }

// Kind reports that this core is a server rather than a proxy.
func (f *Fabric) Kind() Kind { return KindServer }

// Runtime reports what has to be installed to run this core.
func (f *Fabric) Runtime() Runtime { return RuntimeJava }

// Content reports where this core's add-ons go. Fabric reads mods, not
// plugins: a Bukkit plugin dropped beside it is never loaded.
func (f *Fabric) Content() Content {
	return Content{Loader: "fabric", Dir: "mods"}
}

// fabricGameVersion is an entry of /versions/game.
type fabricGameVersion struct {
	Version string `json:"version"`
	Stable  bool   `json:"stable"`
}

// fabricComponent is an entry of /versions/loader or /versions/installer.
type fabricComponent struct {
	Version string `json:"version"`
	Stable  bool   `json:"stable"`
}

// Versions lists the Minecraft versions Fabric supports, newest first.
//
// Fabric returns them newest first already, and its own "stable" flag is
// authoritative: it marks the game versions Fabric considers releases, which
// is not the same question as whether the id looks like one.
func (f *Fabric) Versions(ctx context.Context) ([]Version, error) {
	var games []fabricGameVersion
	if err := getJSON(ctx, f.Client, f.base()+"/versions/game", &games); err != nil {
		return nil, fmt.Errorf("reading fabric game versions: %w", err)
	}

	out := make([]Version, 0, len(games))
	for _, game := range games {
		channel := ChannelSnapshot
		if game.Stable {
			channel = ChannelRelease
		}
		out = append(out, Version{
			ID:        game.Version,
			Channel:   channel,
			JavaMajor: JavaMajorFor(game.Version),
		})
	}
	return out, nil
}

// Resolve builds the download for a game version.
//
// The URL carries three versions: the game, the loader and the installer. The
// newest stable of the latter two is used, which is what the Fabric installer
// itself would pick.
func (f *Fabric) Resolve(ctx context.Context, version string) (*Build, error) {
	versions, err := f.Versions(ctx)
	if err != nil {
		return nil, err
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("%w: fabric publishes no versions", ErrUnknownVersion)
	}

	if version == "" {
		version = newestRelease(versions)
	}

	known := false
	for _, v := range versions {
		if v.ID == version {
			known = true
			break
		}
	}
	if !known {
		return nil, fmt.Errorf("%w: fabric %s", ErrUnknownVersion, version)
	}

	loader, installer, err := f.components(ctx)
	if err != nil {
		return nil, err
	}

	return &Build{
		Core:    f.ID(),
		Version: version,
		// The loader version is what distinguishes two downloads for the same
		// Minecraft version, so it is the build id.
		Build: loader,
		URL: fmt.Sprintf("%s/versions/loader/%s/%s/%s/server/jar",
			f.base(), version, loader, installer),
		FileName: fmt.Sprintf("fabric-server-mc.%s-loader.%s-launcher.%s.jar",
			version, loader, installer),
		// Under two hundred kilobytes: this jar is not the server, it is the
		// thing that downloads the server and the loader's libraries the
		// first time it runs.
		Artifact: ArtifactLauncher,
		// Fabric publishes no checksum for this endpoint. Recorded by its
		// absence rather than by pretending: the download is over TLS and
		// that is all it can promise.
		JavaMajor: JavaMajorFor(version),
	}, nil
}

// components returns the newest stable loader and installer versions.
func (f *Fabric) components(ctx context.Context) (loader, installer string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.loader != "" && f.installer != "" && f.now().Sub(f.cachedAt) < fabricCacheTTL {
		return f.loader, f.installer, nil
	}

	var loaders []fabricComponent
	if err := getJSON(ctx, f.Client, f.base()+"/versions/loader", &loaders); err != nil {
		return "", "", fmt.Errorf("reading fabric loader versions: %w", err)
	}
	var installers []fabricComponent
	if err := getJSON(ctx, f.Client, f.base()+"/versions/installer", &installers); err != nil {
		return "", "", fmt.Errorf("reading fabric installer versions: %w", err)
	}

	loader = newestStable(loaders)
	installer = newestStable(installers)
	if loader == "" || installer == "" {
		return "", "", fmt.Errorf("%w: fabric publishes no stable loader or installer", ErrNoBuilds)
	}

	f.loader, f.installer, f.cachedAt = loader, installer, f.now()
	return loader, installer, nil
}

// newestStable returns the first stable entry, falling back to the first of
// any: a project that has published only pre-releases has nothing else to
// offer, and refusing would be worse than saying so.
func newestStable(components []fabricComponent) string {
	for _, c := range components {
		if c.Stable {
			return c.Version
		}
	}
	if len(components) > 0 {
		return components[0].Version
	}
	return ""
}

func (f *Fabric) base() string {
	if f.BaseURL == "" {
		return FabricMetaBase
	}
	return f.BaseURL
}
