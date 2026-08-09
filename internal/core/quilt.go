package core

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// QuiltMetaBase is the Quilt metadata API root.
const QuiltMetaBase = "https://meta.quiltmc.org/v3"

// quiltCacheTTL is how long the installer list is reused.
const quiltCacheTTL = time.Hour

// quiltServerJar is what Quilt's installer leaves behind.
const quiltServerJar = "quilt-server-launch.jar"

// Quilt serves the Quilt mod loader.
//
// Unlike Fabric, which it forked from, Quilt publishes no ready-made server
// launcher: there is an installer that has to be run. That is the whole
// difference between the two providers, and it is why this one is not three
// lines of Fabric with a different URL.
type Quilt struct {
	// BaseURL is overridable for tests.
	BaseURL string
	Client  *http.Client

	// Games supplies the Minecraft versions. Quilt's own game list mirrors
	// Fabric's, so the Fabric provider answers it rather than this one asking
	// the same question of a second service.
	Games *Fabric

	mu         sync.Mutex
	installer  string
	installURL string
	cachedAt   time.Time
	now        func() time.Time
}

// NewQuilt returns the Quilt provider.
func NewQuilt(client *http.Client) *Quilt {
	return &Quilt{
		BaseURL: QuiltMetaBase,
		Client:  client,
		Games:   NewFabric(client),
		now:     time.Now,
	}
}

// ID returns the identifier the API and the database use for this core.
func (q *Quilt) ID() string { return "quilt" }

// Name returns the name shown in the panel.
func (q *Quilt) Name() string { return "Quilt" }

// Kind reports that this core is a server rather than a proxy.
func (q *Quilt) Kind() Kind { return KindServer }

// Runtime reports what has to be installed to run this core.
func (q *Quilt) Runtime() Runtime { return RuntimeJava }

// Content reports where this core's add-ons go.
func (q *Quilt) Content() Content {
	return Content{Loader: "quilt", Dir: "mods"}
}

// quiltComponent is an entry of /versions/installer.
type quiltComponent struct {
	Version string `json:"version"`
	URL     string `json:"url"`
}

// Versions lists the Minecraft versions Quilt can install.
func (q *Quilt) Versions(ctx context.Context) ([]Version, error) {
	return q.Games.Versions(ctx)
}

// Resolve returns the installer for a Minecraft version.
func (q *Quilt) Resolve(ctx context.Context, version string) (*Build, error) {
	versions, err := q.Versions(ctx)
	if err != nil {
		return nil, err
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("%w: quilt publishes no versions", ErrUnknownVersion)
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
		return nil, fmt.Errorf("%w: quilt %s", ErrUnknownVersion, version)
	}

	installer, url, err := q.installerVersion(ctx)
	if err != nil {
		return nil, err
	}

	return &Build{
		Core:      q.ID(),
		Version:   version,
		Build:     installer,
		URL:       url,
		FileName:  fmt.Sprintf("quilt-installer-%s.jar", installer),
		Artifact:  ArtifactInstaller,
		JavaMajor: JavaMajorFor(version),
	}, nil
}

// InstallArgs installs a server for the build's Minecraft version.
//
// --download-server fetches the Minecraft server itself, so the result runs
// without a further download; without it the first start fails looking for a
// jar nobody put there.
func (q *Quilt) InstallArgs(build *Build) []string {
	return []string{"install", "server", build.Version, "--download-server", "--install-dir=."}
}

// LaunchArgs returns the launcher the installer wrote.
func (q *Quilt) LaunchArgs(dir string, _ *Build, _ string) ([]string, error) {
	if _, err := os.Stat(filepath.Join(dir, quiltServerJar)); err != nil {
		return nil, fmt.Errorf("the quilt installer left no %s: %w", quiltServerJar, err)
	}
	return []string{"-jar", quiltServerJar, "nogui"}, nil
}

// installerVersion returns the newest installer and where to get it.
func (q *Quilt) installerVersion(ctx context.Context) (version, url string, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.installer != "" && q.now().Sub(q.cachedAt) < quiltCacheTTL {
		return q.installer, q.installURL, nil
	}

	var installers []quiltComponent
	if err := getJSON(ctx, q.Client, q.base()+"/versions/installer", &installers); err != nil {
		return "", "", fmt.Errorf("reading quilt installer versions: %w", err)
	}
	if len(installers) == 0 || installers[0].URL == "" {
		return "", "", fmt.Errorf("%w: quilt publishes no installer", ErrNoBuilds)
	}

	q.installer, q.installURL, q.cachedAt = installers[0].Version, installers[0].URL, q.now()
	return q.installer, q.installURL, nil
}

func (q *Quilt) base() string {
	if q.BaseURL == "" {
		return QuiltMetaBase
	}
	return q.BaseURL
}
