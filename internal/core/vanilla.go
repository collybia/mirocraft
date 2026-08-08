package core

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"
)

// MojangManifestURL is the version index Mojang publishes.
const MojangManifestURL = "https://piston-meta.mojang.com/mc/game/version_manifest_v2.json"

// manifestTTL is how long the version index is reused. Mojang publishes a
// snapshot roughly weekly, so re-fetching per request would be pointless
// traffic and would make the panel feel slow for no gain.
const manifestTTL = 15 * time.Minute

// Vanilla serves Mojang's own server jars.
type Vanilla struct {
	// ManifestURL is overridable for tests.
	ManifestURL string
	Client      *http.Client

	mu       sync.Mutex
	cached   *mojangManifest
	cachedAt time.Time
	now      func() time.Time
}

// NewVanilla returns the vanilla provider.
func NewVanilla(client *http.Client) *Vanilla {
	return &Vanilla{
		ManifestURL: MojangManifestURL,
		Client:      client,
		now:         time.Now,
	}
}

func (v *Vanilla) ID() string       { return "vanilla" }
func (v *Vanilla) Name() string     { return "Vanilla" }
func (v *Vanilla) Kind() Kind       { return KindServer }
func (v *Vanilla) Runtime() Runtime { return RuntimeJava }

// Content is empty: a vanilla server has no plugin or mod loader.
//
// Said plainly rather than defaulting to plugins/, so the panel can refuse an
// install with a reason. A jar dropped beside a vanilla server is never read,
// and "installed successfully, does nothing" is the worst possible answer.
func (v *Vanilla) Content() Content { return Content{} }

// mojangManifest is the index of every published version.
type mojangManifest struct {
	Latest struct {
		Release  string `json:"release"`
		Snapshot string `json:"snapshot"`
	} `json:"latest"`
	Versions []mojangVersion `json:"versions"`
}

type mojangVersion struct {
	ID          string    `json:"id"`
	Type        string    `json:"type"`
	URL         string    `json:"url"`
	ReleaseTime time.Time `json:"releaseTime"`
}

// mojangVersionDetail is the per-version document the index points at.
type mojangVersionDetail struct {
	Downloads struct {
		Server struct {
			SHA1 string `json:"sha1"`
			Size int64  `json:"size"`
			URL  string `json:"url"`
		} `json:"server"`
	} `json:"downloads"`
	JavaVersion struct {
		MajorVersion int `json:"majorVersion"`
	} `json:"javaVersion"`
}

// Versions lists releases and snapshots, newest first.
func (v *Vanilla) Versions(ctx context.Context) ([]Version, error) {
	manifest, err := v.manifest(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]Version, 0, len(manifest.Versions))
	for _, mv := range manifest.Versions {
		channel := ChannelSnapshot
		if mv.Type == "release" {
			channel = ChannelRelease
		}
		out = append(out, Version{
			ID:         mv.ID,
			Channel:    channel,
			ReleasedAt: mv.ReleaseTime,
			JavaMajor:  JavaMajorFor(mv.ID),
		})
	}

	// Mojang already orders the manifest newest first, but that is a property
	// of their document rather than a promise, so it is enforced here.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].ReleasedAt.After(out[j].ReleasedAt)
	})
	return out, nil
}

// Resolve returns the server jar for a version. An empty version means the
// latest release — never the latest snapshot, which would silently put people
// on unstable builds.
func (v *Vanilla) Resolve(ctx context.Context, version string) (*Build, error) {
	manifest, err := v.manifest(ctx)
	if err != nil {
		return nil, err
	}

	if version == "" {
		version = manifest.Latest.Release
	}

	var entry *mojangVersion
	for i := range manifest.Versions {
		if manifest.Versions[i].ID == version {
			entry = &manifest.Versions[i]
			break
		}
	}
	if entry == nil {
		return nil, fmt.Errorf("%w: vanilla %s", ErrUnknownVersion, version)
	}

	var detail mojangVersionDetail
	if err := getJSON(ctx, v.Client, entry.URL, &detail); err != nil {
		return nil, fmt.Errorf("reading the version document for %s: %w", version, err)
	}
	if detail.Downloads.Server.URL == "" {
		// Some very old versions predate a published server jar.
		return nil, fmt.Errorf("%w: vanilla %s publishes no server jar", ErrUnknownVersion, version)
	}

	// Mojang states the Java requirement per version, which cannot go stale
	// the way a table in this repository can, so it wins where present.
	javaMajor := detail.JavaVersion.MajorVersion
	if javaMajor == 0 {
		javaMajor = JavaMajorFor(version)
	}

	return &Build{
		Core:      v.ID(),
		Version:   version,
		URL:       detail.Downloads.Server.URL,
		FileName:  "server.jar",
		Checksum:  detail.Downloads.Server.SHA1,
		Algorithm: AlgoSHA1,
		SizeBytes: detail.Downloads.Server.Size,
		JavaMajor: javaMajor,
	}, nil
}

// manifest returns the version index, refreshing it when stale.
func (v *Vanilla) manifest(ctx context.Context) (*mojangManifest, error) {
	v.mu.Lock()
	defer v.mu.Unlock()

	now := time.Now
	if v.now != nil {
		now = v.now
	}
	if v.cached != nil && now().Sub(v.cachedAt) < manifestTTL {
		return v.cached, nil
	}

	url := v.ManifestURL
	if url == "" {
		url = MojangManifestURL
	}

	var manifest mojangManifest
	if err := getJSON(ctx, v.Client, url, &manifest); err != nil {
		// A stale manifest beats no manifest: the panel stays usable when
		// Mojang is briefly unreachable, and versions do not disappear.
		if v.cached != nil {
			return v.cached, nil
		}
		return nil, fmt.Errorf("reading the Mojang manifest: %w", err)
	}

	v.cached = &manifest
	v.cachedAt = now()
	return v.cached, nil
}

// decodeJSON is shared by the providers so the size limit and the strictness
// setting live in one place.
func decodeJSON(r io.Reader, target any) error {
	return json.NewDecoder(r).Decode(target)
}
