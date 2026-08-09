package core

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

// SpongeAPIBase is the SpongePowered download API.
const SpongeAPIBase = "https://dl-api.spongepowered.org/v2"

// spongeCacheTTL is how long the version listing is reused. It runs to three
// thousand entries, and reading it on every page load would be rude to a
// service that asks nothing in return.
const spongeCacheTTL = 30 * time.Minute

// spongePageSize is how many versions are read. The API pages, newest first,
// and a few hundred covers every Minecraft version anyone still runs — the
// rest are release candidates of versions long superseded.
const spongePageSize = 300

// spongeUniversal is the classifier of the runnable artifact. The same version
// publishes sources, javadoc and several development jars; picking by
// extension alone would install one of those.
const spongeUniversal = "universal"

// Sponge serves SpongeVanilla, the standalone Sponge server.
//
// SpongeForge is not here: it is a mod installed into a Forge server rather
// than a server of its own, so it belongs to the add-on catalogue and not to
// the list of things a server can be created as.
type Sponge struct {
	// BaseURL is overridable for tests.
	BaseURL string
	Client  *http.Client

	mu       sync.Mutex
	cached   map[string]string // minecraft version -> newest sponge version
	order    []string
	cachedAt time.Time
	now      func() time.Time
}

// NewSponge returns the Sponge provider.
func NewSponge(client *http.Client) *Sponge {
	return &Sponge{BaseURL: SpongeAPIBase, Client: client, now: time.Now}
}

// ID returns the identifier the API and the database use for this core.
func (s *Sponge) ID() string { return "sponge" }

// Name returns the name shown in the panel.
func (s *Sponge) Name() string { return "SpongeVanilla" }

// Kind reports that this core is a server rather than a proxy.
func (s *Sponge) Kind() Kind { return KindServer }

// Runtime reports what has to be installed to run this core.
func (s *Sponge) Runtime() Runtime { return RuntimeJava }

// Content reports where this core's add-ons go. Sponge plugins are their own
// thing: a Bukkit plugin does not run here and a Sponge plugin does not run on
// Paper, which is why the loader is neither.
func (s *Sponge) Content() Content {
	return Content{Loader: "sponge", Dir: "mods"}
}

// spongeVersions is the version listing.
type spongeVersions struct {
	Artifacts map[string]struct {
		Recommended bool `json:"recommended"`
		TagValues   struct {
			API       string `json:"api"`
			Minecraft string `json:"minecraft"`
		} `json:"tagValues"`
	} `json:"artifacts"`
}

// spongeVersion is one version's detail.
type spongeVersion struct {
	Assets []struct {
		Classifier  string `json:"classifier"`
		DownloadURL string `json:"downloadUrl"`
		Extension   string `json:"extension"`
		SHA1        string `json:"sha1"`
	} `json:"assets"`
}

// Versions lists the Minecraft versions Sponge supports, newest first.
func (s *Sponge) Versions(ctx context.Context) ([]Version, error) {
	byMinecraft, order, err := s.index(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]Version, 0, len(byMinecraft))
	for _, minecraft := range order {
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

// Resolve picks the newest Sponge build for a Minecraft version.
func (s *Sponge) Resolve(ctx context.Context, version string) (*Build, error) {
	byMinecraft, _, err := s.index(ctx)
	if err != nil {
		return nil, err
	}

	if version == "" {
		versions, err := s.Versions(ctx)
		if err != nil {
			return nil, err
		}
		if len(versions) == 0 {
			return nil, fmt.Errorf("%w: sponge publishes no versions", ErrUnknownVersion)
		}
		version = newestRelease(versions)
	}

	spongeVer, ok := byMinecraft[version]
	if !ok {
		return nil, fmt.Errorf("%w: sponge %s", ErrUnknownVersion, version)
	}

	var detail spongeVersion
	url := fmt.Sprintf("%s/groups/org.spongepowered/artifacts/spongevanilla/versions/%s", s.base(), spongeVer)
	if err := getJSON(ctx, s.Client, url, &detail); err != nil {
		return nil, fmt.Errorf("reading sponge %s: %w", spongeVer, err)
	}

	for _, asset := range detail.Assets {
		if asset.Classifier != spongeUniversal || asset.Extension != "jar" {
			continue
		}
		return &Build{
			Core:      s.ID(),
			Version:   version,
			Build:     spongeVer,
			URL:       asset.DownloadURL,
			FileName:  fmt.Sprintf("spongevanilla-%s.jar", spongeVer),
			Checksum:  asset.SHA1,
			Algorithm: AlgoSHA1,
			JavaMajor: JavaMajorFor(version),
		}, nil
	}

	return nil, fmt.Errorf("%w: sponge %s publishes no %s jar", ErrNoBuilds, spongeVer, spongeUniversal)
}

// index maps each Minecraft version to the newest Sponge version for it.
func (s *Sponge) index(ctx context.Context) (map[string]string, []string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cached != nil && s.now().Sub(s.cachedAt) < spongeCacheTTL {
		return s.cached, s.order, nil
	}

	var listing spongeVersions
	url := fmt.Sprintf("%s/groups/org.spongepowered/artifacts/spongevanilla/versions?limit=%d",
		s.base(), spongePageSize)
	if err := getJSON(ctx, s.Client, url, &listing); err != nil {
		return nil, nil, fmt.Errorf("reading sponge versions: %w", err)
	}

	// The listing is newest first, so the first entry seen for a Minecraft
	// version is the newest Sponge build for it.
	byMinecraft := make(map[string]string, len(listing.Artifacts))
	var order []string
	for spongeVer, entry := range listing.Artifacts {
		minecraft := entry.TagValues.Minecraft
		if minecraft == "" {
			continue
		}
		if existing, ok := byMinecraft[minecraft]; ok {
			// Map iteration is random, so "first seen" is not a rule here:
			// the newer version string wins explicitly.
			if CompareVersions(existing, spongeVer) >= 0 {
				continue
			}
		} else {
			order = append(order, minecraft)
		}
		byMinecraft[minecraft] = spongeVer
	}

	s.cached, s.order, s.cachedAt = byMinecraft, order, s.now()
	return s.cached, s.order, nil
}

func (s *Sponge) base() string {
	if s.BaseURL == "" {
		return SpongeAPIBase
	}
	return s.BaseURL
}
