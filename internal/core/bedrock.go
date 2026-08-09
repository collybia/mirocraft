package core

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// BedrockLinksAPI is where Mojang publishes the current server downloads.
//
// Their own JSON rather than the download page: the page is HTML that changes
// shape with the marketing, and a provider built on scraping it breaks on a
// redesign nobody announced.
const BedrockLinksAPI = "https://net-secondary.web.minecraft-services.net/api/v1.0/download/links"

// bedrockCacheTTL is how long the link list is reused.
const bedrockCacheTTL = 30 * time.Minute

// The download types Mojang publishes. Preview builds are deliberately not
// offered: they are for testing against the next protocol, and a player on the
// release client cannot join one.
const (
	bedrockLinux   = "serverBedrockLinux"
	bedrockWindows = "serverBedrockWindows"
)

// BedrockUserAgent is what Mojang's CDN accepts. Not a claim to be a browser
// for its own sake: their edge drops requests with an unfamiliar agent, and
// the alternative to this line is a Bedrock server that can never be
// downloaded.
const BedrockUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64)"

// bedrockVersion pulls the version out of the file name, which is the only
// place Mojang states it: bedrock-server-1.26.43.1.zip.
var bedrockVersion = regexp.MustCompile(`bedrock-server-([0-9][0-9.]*)\.zip`)

// Bedrock serves Mojang's Bedrock Dedicated Server.
//
// Nothing about it is like a Java server. It is a native executable rather
// than a jar, it speaks UDP rather than TCP, and its licence forbids
// redistribution — which is why the panel fetches it from Mojang at install
// time rather than mirroring it.
//
// Only the current version is offered, because that is all Mojang publishes:
// they keep no archive of older builds, and Bedrock clients update themselves
// and refuse to join an older server anyway.
type Bedrock struct {
	// LinksURL is overridable for tests.
	LinksURL string
	Client   *http.Client

	// TargetOS is the platform to fetch for, "linux" or "windows". Empty means
	// this host's — which is right, because the archive holds a native binary
	// and the one that runs is the one for the machine it runs on.
	TargetOS string

	mu       sync.Mutex
	cached   map[string]bedrockLink
	cachedAt time.Time
	now      func() time.Time
}

type bedrockLink struct {
	URL     string
	Version string
}

// NewBedrock returns the Bedrock provider.
func NewBedrock(client *http.Client) *Bedrock {
	return &Bedrock{LinksURL: BedrockLinksAPI, Client: client, now: time.Now}
}

// ID returns the identifier the API and the database use for this core.
func (b *Bedrock) ID() string { return "bedrock" }

// Name returns the name shown in the panel.
func (b *Bedrock) Name() string { return "Bedrock Dedicated Server" }

// Kind reports that this is a Bedrock server: a different protocol, a
// different default port and a different transport from a Java one.
func (b *Bedrock) Kind() Kind { return KindBedrock }

// Runtime reports that this is a native executable — no Java involved.
func (b *Bedrock) Runtime() Runtime { return RuntimeNative }

// Content reports that this core takes no add-ons the catalogue knows about.
//
// Bedrock has behaviour and resource packs rather than plugins, and the
// registries the panel searches do not index them. Said plainly rather than
// pointing at a folder that would stay empty.
func (b *Bedrock) Content() Content { return Content{} }

// bedrockLinks is Mojang's document.
type bedrockLinks struct {
	Result struct {
		Links []struct {
			DownloadType string `json:"downloadType"`
			DownloadURL  string `json:"downloadUrl"`
		} `json:"links"`
	} `json:"result"`
}

// Versions lists what Mojang currently publishes, which is one version.
func (b *Bedrock) Versions(ctx context.Context) ([]Version, error) {
	links, err := b.links(ctx)
	if err != nil {
		return nil, err
	}

	link, ok := links[b.downloadType()]
	if !ok {
		return nil, fmt.Errorf("%w: mojang publishes no bedrock server for %s",
			ErrUnsupportedPlatform, b.targetOS())
	}
	return []Version{{
		ID:      link.Version,
		Channel: ChannelRelease,
		// Native: no Java at all, and the field is what the runner reads to
		// pick an image, so zero is the honest answer rather than a default.
		JavaMajor: 0,
	}}, nil
}

// Resolve returns the current build.
func (b *Bedrock) Resolve(ctx context.Context, version string) (*Build, error) {
	links, err := b.links(ctx)
	if err != nil {
		return nil, err
	}

	link, ok := links[b.downloadType()]
	if !ok {
		return nil, fmt.Errorf("%w: mojang publishes no bedrock server for %s",
			ErrUnsupportedPlatform, b.targetOS())
	}
	if version != "" && version != link.Version {
		return nil, fmt.Errorf("%w: mojang publishes only the current bedrock server, %s",
			ErrUnknownVersion, link.Version)
	}

	return &Build{
		Core:     b.ID(),
		Version:  link.Version,
		URL:      link.URL,
		FileName: fmt.Sprintf("bedrock-server-%s-%s.zip", link.Version, b.targetOS()),
		// Mojang publishes no checksum beside the download.
		Artifact: ArtifactArchive,
		// Mojang's CDN refuses this panel's user agent: the connection is
		// closed mid-stream, which arrives as an HTTP/2 internal error rather
		// than as a rejection, so it reads like a network fault. A browser
		// agent is accepted, and so is none — this is the smallest thing that
		// works and it is used only for this one host.
		UserAgent: BedrockUserAgent,
	}, nil
}

// links reads Mojang's document, refreshing when stale.
func (b *Bedrock) links(ctx context.Context) (map[string]bedrockLink, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.cached != nil && b.now().Sub(b.cachedAt) < bedrockCacheTTL {
		return b.cached, nil
	}

	var document bedrockLinks
	if err := getJSON(ctx, b.Client, b.linksURL(), &document); err != nil {
		return nil, fmt.Errorf("reading the bedrock download links: %w", err)
	}

	out := make(map[string]bedrockLink, len(document.Result.Links))
	for _, link := range document.Result.Links {
		match := bedrockVersion.FindStringSubmatch(link.DownloadURL)
		if match == nil {
			continue
		}
		out[link.DownloadType] = bedrockLink{URL: link.DownloadURL, Version: match[1]}
	}

	b.cached, b.cachedAt = out, b.now()
	return b.cached, nil
}

// downloadType is Mojang's name for the archive this platform needs.
func (b *Bedrock) downloadType() string {
	if b.targetOS() == TargetWindows {
		return bedrockWindows
	}
	return bedrockLinux
}

func (b *Bedrock) targetOS() string {
	if b.TargetOS != "" {
		return strings.ToLower(b.TargetOS)
	}
	return runtime.GOOS
}

func (b *Bedrock) linksURL() string {
	if b.LinksURL == "" {
		return BedrockLinksAPI
	}
	return b.LinksURL
}

// BedrockExecutable is the file the archive unpacks to.
func BedrockExecutable(targetOS string) string {
	if targetOS == TargetWindows {
		return "bedrock_server.exe"
	}
	return "bedrock_server"
}
