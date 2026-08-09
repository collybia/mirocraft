package core

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ForgePromotions is where Forge publishes which build is current per version.
const ForgePromotions = "https://files.minecraftforge.net/net/minecraftforge/forge/promotions_slim.json"

// ForgeMavenBase is where the installers live.
const ForgeMavenBase = "https://maven.minecraftforge.net"

// forgeCacheTTL is how long the promotions document is reused.
const forgeCacheTTL = 30 * time.Minute

// Forge serves MinecraftForge.
//
// Like NeoForge, the download is an installer. Unlike it, Forge spans the
// change at 1.17, where it moved to the modular JVM and stopped leaving a
// runnable server jar: older versions produce forge-<mc>-<build>.jar and newer
// ones an argument file. LaunchArgs looks at which is there rather than
// deriving it from the version, because the installer is the authority and a
// version rule would be wrong for exactly the releases around the boundary.
type Forge struct {
	// PromotionsURL and MavenURL are overridable for tests.
	PromotionsURL string
	MavenURL      string
	Client        *http.Client

	mu       sync.Mutex
	cached   map[string]string
	cachedAt time.Time
	now      func() time.Time
}

// NewForge returns the Forge provider.
func NewForge(client *http.Client) *Forge {
	return &Forge{
		PromotionsURL: ForgePromotions,
		MavenURL:      ForgeMavenBase,
		Client:        client,
		now:           time.Now,
	}
}

// ID returns the identifier the API and the database use for this core.
func (f *Forge) ID() string { return "forge" }

// Name returns the name shown in the panel.
func (f *Forge) Name() string { return "Forge" }

// Kind reports that this core is a server rather than a proxy.
func (f *Forge) Kind() Kind { return KindServer }

// Runtime reports what has to be installed to run this core.
func (f *Forge) Runtime() Runtime { return RuntimeJava }

// Content reports where this core's add-ons go.
func (f *Forge) Content() Content {
	return Content{Loader: "forge", Dir: "mods"}
}

// forgePromotions is the promotions document: {"promos": {"1.21.4-latest":
// "54.1.0", "1.21.4-recommended": "54.0.16", ...}}.
type forgePromotions struct {
	Promos map[string]string `json:"promos"`
}

// Versions lists the Minecraft versions Forge publishes a build for.
func (f *Forge) Versions(ctx context.Context) ([]Version, error) {
	promos, err := f.promotions(ctx)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool, len(promos))
	out := make([]Version, 0, len(promos)/2)
	for key := range promos {
		minecraft, _, ok := splitPromotion(key)
		if !ok || seen[minecraft] {
			continue
		}
		seen[minecraft] = true
		out = append(out, Version{
			ID:        minecraft,
			Channel:   ChannelRelease,
			JavaMajor: JavaMajorFor(minecraft),
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		return CompareVersions(out[i].ID, out[j].ID) > 0
	})
	return out, nil
}

// Resolve picks the recommended build for a version, falling back to latest.
//
// Recommended first because that is Forge's own word for the build modpacks
// are built against; "latest" is the newest, which is not the same thing and
// is regularly the one with a fresh regression.
func (f *Forge) Resolve(ctx context.Context, version string) (*Build, error) {
	promos, err := f.promotions(ctx)
	if err != nil {
		return nil, err
	}

	if version == "" {
		versions, err := f.Versions(ctx)
		if err != nil {
			return nil, err
		}
		if len(versions) == 0 {
			return nil, fmt.Errorf("%w: forge publishes no versions", ErrUnknownVersion)
		}
		version = versions[0].ID
	}

	build, ok := promos[version+"-recommended"]
	if !ok {
		build, ok = promos[version+"-latest"]
	}
	if !ok {
		return nil, fmt.Errorf("%w: forge %s", ErrUnknownVersion, version)
	}

	full := version + "-" + build
	return &Build{
		Core:    f.ID(),
		Version: version,
		Build:   build,
		URL: fmt.Sprintf("%s/net/minecraftforge/forge/%s/forge-%s-installer.jar",
			f.maven(), full, full),
		FileName:  fmt.Sprintf("forge-%s-installer.jar", full),
		Artifact:  ArtifactInstaller,
		JavaMajor: JavaMajorFor(version),
	}, nil
}

// InstallArgs runs the installer in server mode.
func (f *Forge) InstallArgs(*Build) []string {
	return []string{"--installServer", "."}
}

// LaunchArgs works out how this Forge server starts.
//
// Forge changed shape at 1.17: before it, the installer leaves a runnable
// forge-<mc>-<build>.jar; after it, an argument file listing the classpath.
// Which one is there is checked rather than derived from the version, because
// the installer is the authority and a version-based rule would be wrong for
// exactly the releases around the boundary.
func (f *Forge) LaunchArgs(dir string, build *Build, targetOS string) ([]string, error) {
	name := "unix_args.txt"
	if targetOS == TargetWindows {
		name = "win_args.txt"
	}
	full := build.Version + "-" + build.Build
	argsFile := filepath.ToSlash(filepath.Join("libraries", "net", "minecraftforge", "forge", full, name))

	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(argsFile))); err == nil {
		return []string{"@" + argsFile, "nogui"}, nil
	}

	// The older shape: a runnable jar beside the installer.
	jar := fmt.Sprintf("forge-%s.jar", full)
	if _, err := os.Stat(filepath.Join(dir, jar)); err == nil {
		return []string{"-jar", jar, "nogui"}, nil
	}
	// Some versions name it with a -universal suffix instead.
	universal := fmt.Sprintf("forge-%s-universal.jar", full)
	if _, err := os.Stat(filepath.Join(dir, universal)); err == nil {
		return []string{"-jar", universal, "nogui"}, nil
	}

	return nil, fmt.Errorf("the forge installer left neither %s nor %s in %s", argsFile, jar, dir)
}

// promotions reads the promotions document, refreshing when stale.
func (f *Forge) promotions(ctx context.Context) (map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.cached != nil && f.now().Sub(f.cachedAt) < forgeCacheTTL {
		return f.cached, nil
	}

	var document forgePromotions
	if err := getJSON(ctx, f.Client, f.promotionsURL(), &document); err != nil {
		return nil, fmt.Errorf("reading forge promotions: %w", err)
	}

	f.cached, f.cachedAt = document.Promos, f.now()
	return f.cached, nil
}

// splitPromotion splits "1.21.4-recommended" into its parts.
func splitPromotion(key string) (minecraft, channel string, ok bool) {
	at := strings.LastIndex(key, "-")
	if at <= 0 {
		return "", "", false
	}
	minecraft, channel = key[:at], key[at+1:]
	if channel != "recommended" && channel != "latest" {
		return "", "", false
	}
	return minecraft, channel, true
}

func (f *Forge) promotionsURL() string {
	if f.PromotionsURL == "" {
		return ForgePromotions
	}
	return f.PromotionsURL
}

func (f *Forge) maven() string {
	if f.MavenURL == "" {
		return ForgeMavenBase
	}
	return f.MavenURL
}
