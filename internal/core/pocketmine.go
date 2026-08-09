package core

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// PocketMineAPI reports the current build of a channel.
const PocketMineAPI = "https://update.pmmp.io/api"

// pocketMineCacheTTL is how long the current build is reused.
const pocketMineCacheTTL = 30 * time.Minute

// PocketMine serves PocketMine-MP, a Bedrock server written in PHP.
//
// What it publishes is a phar — a PHP archive — and the PHP it needs is not
// the one a distribution ships: PocketMine builds its own with the extensions
// it requires, and running it on a stock PHP fails on a missing extension
// rather than on anything a person would recognise as the cause. So the
// runtime comes from them too, and its version is whatever this build asks
// for.
type PocketMine struct {
	// APIURL is overridable for tests.
	APIURL string
	Client *http.Client

	mu       sync.Mutex
	cached   *pocketMineBuild
	cachedAt time.Time
	now      func() time.Time
}

// pocketMineBuild is the API's answer.
type pocketMineBuild struct {
	PHPVersion  string `json:"php_version"`
	BaseVersion string `json:"base_version"`
	Build       int    `json:"build"`
	MCPEVersion string `json:"mcpe_version"`
	DownloadURL string `json:"download_url"`
}

// NewPocketMine returns the PocketMine provider.
func NewPocketMine(client *http.Client) *PocketMine {
	return &PocketMine{APIURL: PocketMineAPI, Client: client, now: time.Now}
}

// ID returns the identifier the API and the database use for this core.
func (p *PocketMine) ID() string { return "pocketmine" }

// Name returns the name shown in the panel.
func (p *PocketMine) Name() string { return "PocketMine-MP" }

// Kind reports that players connect with a Bedrock client.
func (p *PocketMine) Kind() Kind { return KindBedrock }

// Runtime reports that this one needs PHP rather than Java.
func (p *PocketMine) Runtime() Runtime { return RuntimePHP }

// Content reports where its plugins go. PocketMine plugins are PHP and share
// nothing with the Java world beyond the folder name.
func (p *PocketMine) Content() Content {
	return Content{Loader: "pocketmine", Dir: "plugins"}
}

// Versions lists what PocketMine currently publishes, which is one build per
// channel; only stable is offered.
func (p *PocketMine) Versions(ctx context.Context) ([]Version, error) {
	build, err := p.current(ctx)
	if err != nil {
		return nil, err
	}
	return []Version{{
		ID:      build.BaseVersion,
		Channel: ChannelRelease,
		// No Java: the field is what a Java runner reads, and PocketMine is
		// not one.
		JavaMajor: 0,
	}}, nil
}

// Resolve returns the current phar.
func (p *PocketMine) Resolve(ctx context.Context, version string) (*Build, error) {
	build, err := p.current(ctx)
	if err != nil {
		return nil, err
	}
	if version != "" && version != build.BaseVersion {
		return nil, fmt.Errorf("%w: pocketmine publishes only its current build, %s",
			ErrUnknownVersion, build.BaseVersion)
	}
	if build.DownloadURL == "" {
		return nil, fmt.Errorf("%w: pocketmine %s publishes no download",
			ErrNoBuilds, build.BaseVersion)
	}

	return &Build{
		Core:     p.ID(),
		Version:  build.BaseVersion,
		Build:    fmt.Sprintf("%d", build.Build),
		URL:      build.DownloadURL,
		FileName: PocketMinePhar,
		// Their API states no digest.
	}, nil
}

// PocketMinePhar is what the download is called in a server directory. Its own
// name rather than server.jar: it is not a jar, and calling it one would have
// the Java launcher try to run it.
const PocketMinePhar = "PocketMine-MP.phar"

// PHPVersion is the PHP this build needs, as PocketMine states it.
//
// Asked of the provider rather than derived, because the answer changes with
// the build and getting it wrong means an interpreter that refuses the phar.
func (p *PocketMine) PHPVersion(ctx context.Context) (string, error) {
	build, err := p.current(ctx)
	if err != nil {
		return "", err
	}
	if build.PHPVersion == "" {
		return "", fmt.Errorf("%w: pocketmine did not say which PHP it needs", ErrNoBuilds)
	}
	return build.PHPVersion, nil
}

// current reads the API, refreshing when stale.
func (p *PocketMine) current(ctx context.Context) (*pocketMineBuild, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cached != nil && p.now().Sub(p.cachedAt) < pocketMineCacheTTL {
		return p.cached, nil
	}

	var build pocketMineBuild
	if err := getJSON(ctx, p.Client, p.api()+"?channel=stable", &build); err != nil {
		return nil, fmt.Errorf("reading the current pocketmine build: %w", err)
	}
	if build.BaseVersion == "" {
		return nil, fmt.Errorf("%w: pocketmine published no stable build", ErrNoBuilds)
	}

	p.cached, p.cachedAt = &build, p.now()
	return p.cached, nil
}

func (p *PocketMine) api() string {
	if p.APIURL == "" {
		return PocketMineAPI
	}
	return p.APIURL
}
