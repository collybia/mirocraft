package core

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"
)

// ProxyStopCommand is what a proxy understands as "shut down".
//
// Not "stop", which is what a Minecraft server takes: a proxy given the wrong
// word ignores it, the graceful stop does nothing, and the process is killed
// when the timeout runs out.
const ProxyStopCommand = "end"

// StopCommandFor returns the word a core shuts down on.
func StopCommandFor(kind Kind) string {
	if kind == KindProxy {
		return ProxyStopCommand
	}
	return "stop"
}

// velocityMajor pulls the leading major out of a Velocity version, which is
// its own version rather than a Minecraft one: 3.5.1, 4.0.0, 4.1.0-SNAPSHOT.
var velocityMajor = regexp.MustCompile(`^(\d+)\.`)

// NewVelocity returns the Velocity provider.
//
// Velocity is versioned on its own rather than by Minecraft version: one build
// speaks to a range of them. That is why the Java requirement cannot come from
// the version table, which reads a Minecraft version and would make nonsense
// of "3.5.1".
func NewVelocity(client *http.Client) *Paper {
	p := NewPaperProject("velocity", "Velocity", KindProxy, client)
	p.JavaFor = velocityJava
	return p
}

// velocityJava returns the Java a Velocity version needs.
//
// 3.x asks for 11 and the panel installs 17, the oldest it keeps past 8; 4.x
// requires 21. Read off their own requirements rather than guessed, because a
// proxy that will not start on the runtime the panel picked is indisting-
// uishable from a broken download.
func velocityJava(version string) int {
	match := velocityMajor.FindStringSubmatch(version)
	if match == nil {
		return Java21
	}
	major, err := strconv.Atoi(match[1])
	if err != nil || major >= 4 {
		return Java21
	}
	return Java17
}

// NewWaterfall returns the Waterfall provider.
//
// Waterfall is retired upstream: PaperMC stopped developing it in favour of
// Velocity, and its newest build is for 1.21. It is here because servers still
// run it and a panel that cannot install what an operator already runs is not
// a migration path.
func NewWaterfall(client *http.Client) *Paper {
	return NewPaperProject("waterfall", "Waterfall", KindProxy, client)
}

// BungeeCordCI is the Jenkins that publishes BungeeCord.
const BungeeCordCI = "https://ci.md-5.net/job/BungeeCord"

// BungeeCord serves BungeeCord from its CI.
//
// One artifact, always current: the CI keeps no per-Minecraft-version jobs,
// because BungeeCord's latest build is what supports every version it
// supports. So this provider offers exactly one version, called "latest",
// rather than inventing a version list nobody publishes.
type BungeeCord struct {
	// BaseURL is overridable for tests.
	BaseURL string
	Client  *http.Client
}

// NewBungeeCord returns the BungeeCord provider.
func NewBungeeCord(client *http.Client) *BungeeCord {
	return &BungeeCord{BaseURL: BungeeCordCI, Client: client}
}

// ID returns the identifier the API and the database use for this core.
func (b *BungeeCord) ID() string { return "bungeecord" }

// Name returns the name shown in the panel.
func (b *BungeeCord) Name() string { return "BungeeCord" }

// Kind reports that this core is a proxy.
func (b *BungeeCord) Kind() Kind { return KindProxy }

// Runtime reports what has to be installed to run this core.
func (b *BungeeCord) Runtime() Runtime { return RuntimeJava }

// Content reports where this core's add-ons go. A BungeeCord plugin and a
// Paper plugin are different things that share a folder name, which is why the
// loader is neither of the others.
func (b *BungeeCord) Content() Content {
	return Content{Loader: "bungeecord", Dir: "plugins"}
}

// BungeeCordVersion is the single version this provider offers.
const BungeeCordVersion = "latest"

// Versions lists what BungeeCord publishes, which is one thing.
func (b *BungeeCord) Versions(context.Context) ([]Version, error) {
	return []Version{{
		ID:         BungeeCordVersion,
		Channel:    ChannelRelease,
		JavaMajor:  Java17,
		ReleasedAt: time.Time{},
	}}, nil
}

// Resolve returns the current build.
func (b *BungeeCord) Resolve(_ context.Context, version string) (*Build, error) {
	if version != "" && version != BungeeCordVersion {
		return nil, fmt.Errorf("%w: bungeecord publishes one build, %q, rather than one per minecraft version",
			ErrUnknownVersion, BungeeCordVersion)
	}

	return &Build{
		Core:     b.ID(),
		Version:  BungeeCordVersion,
		URL:      b.base() + "/lastSuccessfulBuild/artifact/bootstrap/target/BungeeCord.jar",
		FileName: "BungeeCord.jar",
		// Jenkins publishes no checksum with the artifact.
		JavaMajor: Java17,
	}, nil
}

func (b *BungeeCord) base() string {
	if b.BaseURL == "" {
		return BungeeCordCI
	}
	return b.BaseURL
}
