package core

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// NeoForgeMavenBase is the NeoForged maven.
const NeoForgeMavenBase = "https://maven.neoforged.net"

// neoForgeCacheTTL is how long the version list is reused.
const neoForgeCacheTTL = 30 * time.Minute

// neoForgeVersion matches a NeoForge version, whose first two parts encode the
// Minecraft version: 21.4.147 is for 1.21.4, and 20.2.19-beta for 1.20.2.
//
// The mapping is NeoForge's own convention rather than a guess: they dropped
// the Minecraft version from the string and made the version number carry it.
var neoForgeVersion = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(-beta)?$`)

// NeoForge serves NeoForge, the Forge fork that took over from it for modern
// Minecraft.
//
// What it publishes is an installer, not a server: running the download does
// nothing useful, and the server it writes starts through an argument file
// rather than with -jar.
type NeoForge struct {
	// BaseURL is overridable for tests.
	BaseURL string
	Client  *http.Client

	mu       sync.Mutex
	cached   []string
	cachedAt time.Time
	now      func() time.Time
}

// NewNeoForge returns the NeoForge provider.
func NewNeoForge(client *http.Client) *NeoForge {
	return &NeoForge{BaseURL: NeoForgeMavenBase, Client: client, now: time.Now}
}

// ID returns the identifier the API and the database use for this core.
func (n *NeoForge) ID() string { return "neoforge" }

// Name returns the name shown in the panel.
func (n *NeoForge) Name() string { return "NeoForge" }

// Kind reports that this core is a server rather than a proxy.
func (n *NeoForge) Kind() Kind { return KindServer }

// Runtime reports what has to be installed to run this core.
func (n *NeoForge) Runtime() Runtime { return RuntimeJava }

// Content reports where this core's add-ons go.
func (n *NeoForge) Content() Content {
	return Content{Loader: "neoforge", Dir: "mods"}
}

// neoForgeVersions is the maven API's version list.
type neoForgeVersions struct {
	Versions []string `json:"versions"`
}

// Versions lists the Minecraft versions NeoForge supports, newest first.
//
// One entry per Minecraft version, not per NeoForge build: an operator picks
// the Minecraft version and wants the newest build for it, and a list of
// several hundred build numbers is a list nobody reads.
func (n *NeoForge) Versions(ctx context.Context) ([]Version, error) {
	all, err := n.versions(ctx)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool, len(all))
	out := make([]Version, 0, 32)
	for _, id := range all {
		minecraft, ok := minecraftForNeoForge(id)
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

// Resolve picks the newest stable NeoForge build for a Minecraft version.
func (n *NeoForge) Resolve(ctx context.Context, version string) (*Build, error) {
	all, err := n.versions(ctx)
	if err != nil {
		return nil, err
	}

	if version == "" {
		versions, err := n.Versions(ctx)
		if err != nil {
			return nil, err
		}
		if len(versions) == 0 {
			return nil, fmt.Errorf("%w: neoforge publishes no versions", ErrUnknownVersion)
		}
		version = versions[0].ID
	}

	// The list is oldest first, so the last match is the newest build. Betas
	// are skipped unless a version has nothing else: someone asking for 1.21.4
	// wants the build people run, not the one being tested.
	best, bestBeta := "", ""
	for _, id := range all {
		minecraft, ok := minecraftForNeoForge(id)
		if !ok || minecraft != version {
			continue
		}
		if strings.HasSuffix(id, "-beta") {
			bestBeta = id
			continue
		}
		best = id
	}
	if best == "" {
		best = bestBeta
	}
	if best == "" {
		return nil, fmt.Errorf("%w: neoforge %s", ErrUnknownVersion, version)
	}

	return &Build{
		Core:    n.ID(),
		Version: version,
		Build:   best,
		URL: fmt.Sprintf("%s/releases/net/neoforged/neoforge/%s/neoforge-%s-installer.jar",
			n.base(), best, best),
		FileName: fmt.Sprintf("neoforge-%s-installer.jar", best),
		// The maven publishes .sha1 files beside the artifact, but as separate
		// requests rather than in the version listing; not fetched, so the
		// download is trusted to TLS like Fabric's.
		Artifact:  ArtifactInstaller,
		JavaMajor: JavaMajorFor(version),
	}, nil
}

// InstallArgs runs the installer in server mode.
//
// --install-server with the current directory: the installer defaults to
// asking through a window, which on a headless VPS means it hangs until the
// timeout rather than failing.
func (n *NeoForge) InstallArgs(*Build) []string {
	return []string{"--install-server", "."}
}

// LaunchArgs returns the argument file the installer wrote.
//
// Modern NeoForge does not ship a runnable server jar at all: the installer
// writes libraries/net/neoforged/neoforge/<version>/{unix,win}_args.txt, which
// lists a classpath of several hundred entries — past what a Windows command
// line holds, which is why they moved to argument files in the first place.
func (n *NeoForge) LaunchArgs(dir string, build *Build, targetOS string) ([]string, error) {
	name := "unix_args.txt"
	if targetOS == TargetWindows {
		name = "win_args.txt"
	}
	relative := filepath.ToSlash(filepath.Join(
		"libraries", "net", "neoforged", "neoforge", build.Build, name))

	if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(relative))); err != nil {
		return nil, fmt.Errorf("the neoforge installer left no %s: %w", relative, err)
	}
	return []string{"@" + relative, "nogui"}, nil
}

// versions reads the maven's version list, refreshing when stale.
func (n *NeoForge) versions(ctx context.Context) ([]string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.cached != nil && n.now().Sub(n.cachedAt) < neoForgeCacheTTL {
		return n.cached, nil
	}

	var listing neoForgeVersions
	url := n.base() + "/api/maven/versions/releases/net/neoforged/neoforge"
	if err := getJSON(ctx, n.Client, url, &listing); err != nil {
		return nil, fmt.Errorf("reading neoforge versions: %w", err)
	}

	n.cached, n.cachedAt = listing.Versions, n.now()
	return n.cached, nil
}

// minecraftForNeoForge derives the Minecraft version from a NeoForge version.
//
// 21.4.147 -> 1.21.4, and 20.2.19-beta -> 1.20.2. A third part of zero means
// the .0 release, which Minecraft spells without it: 21.0.x is 1.21.
func minecraftForNeoForge(version string) (string, bool) {
	match := neoForgeVersion.FindStringSubmatch(version)
	if match == nil {
		return "", false
	}

	major, minor := match[1], match[2]
	if minor == "0" {
		return "1." + major, true
	}
	return "1." + major + "." + minor, true
}

func (n *NeoForge) base() string {
	if n.BaseURL == "" {
		return NeoForgeMavenBase
	}
	return n.BaseURL
}
