package core

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"
)

// BuildToolsURL is where SpigotMC publishes the tool that compiles Spigot.
//
// Spigot is not distributed as a jar and never has been: redistributing it
// would republish Mojang's code, so SpigotMC ships a tool that fetches the
// pieces and assembles them on the machine that will run them. That is why
// this core is compiled rather than downloaded, and why it takes minutes
// rather than seconds.
const BuildToolsURL = "https://hub.spigotmc.org/jenkins/job/BuildTools/lastSuccessfulBuild/artifact/target/BuildTools.jar"

// Spigot serves Spigot, built locally by BuildTools.
//
// The version list comes from Paper's: Paper is a Spigot fork and tracks the
// same Minecraft releases, and asking one service rather than scraping a
// Jenkins for a list SpigotMC does not publish is the honest way to answer
// "which versions can I build".
type Spigot struct {
	// Versions are supplied by this provider. Paper by default.
	Source Provider

	// ToolURL is overridable for tests.
	ToolURL string
	Client  *http.Client
}

// NewSpigot returns the Spigot provider.
func NewSpigot(client *http.Client) *Spigot {
	return &Spigot{Source: NewPaper(client), ToolURL: BuildToolsURL, Client: client}
}

// ID returns the identifier the API and the database use for this core.
func (s *Spigot) ID() string { return "spigot" }

// Name returns the name shown in the panel.
func (s *Spigot) Name() string { return "Spigot" }

// Kind reports that this core is a server rather than a proxy.
func (s *Spigot) Kind() Kind { return KindServer }

// Runtime reports what has to be installed to run this core.
func (s *Spigot) Runtime() Runtime { return RuntimeJava }

// Content reports where this core's add-ons go.
func (s *Spigot) Content() Content {
	return Content{Loader: "spigot", Dir: "plugins"}
}

// Versions lists what can be built.
func (s *Spigot) Versions(ctx context.Context) ([]Version, error) {
	versions, err := s.Source.Versions(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the versions spigot can be built for: %w", err)
	}

	// Only releases. BuildTools takes a --rev of a release; the snapshot ids
	// Paper carries are not revisions it knows.
	out := make([]Version, 0, len(versions))
	for _, version := range versions {
		if version.Channel != ChannelRelease {
			continue
		}
		out = append(out, version)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return CompareVersions(out[i].ID, out[j].ID) > 0
	})
	return out, nil
}

// Resolve describes what would be built for a version.
//
// There is no URL: the artifact does not exist anywhere until this machine
// makes it. What the build carries instead is enough to identify the result,
// so the cache can hold it like any other download.
func (s *Spigot) Resolve(ctx context.Context, version string) (*Build, error) {
	versions, err := s.Versions(ctx)
	if err != nil {
		return nil, err
	}
	if len(versions) == 0 {
		return nil, fmt.Errorf("%w: no versions are available to build spigot for", ErrUnknownVersion)
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
		return nil, fmt.Errorf("%w: spigot %s", ErrUnknownVersion, version)
	}

	return &Build{
		Core:    s.ID(),
		Version: version,
		// No build number: BuildTools produces whatever is current for the
		// revision, and there is no identifier for it until it has run.
		FileName: fmt.Sprintf("spigot-%s.jar", version),
		// Compiled here, so nothing to verify against: the result is only as
		// trustworthy as the sources BuildTools fetched, and it says so.
		Artifact:  ArtifactSource,
		JavaMajor: JavaMajorFor(version),
	}, nil
}

// Tool returns the downloadable tool that does the compiling.
func (s *Spigot) Tool(context.Context) (*Build, error) {
	url := s.ToolURL
	if url == "" {
		url = BuildToolsURL
	}
	return &Build{
		Core:     s.ID(),
		Version:  "buildtools",
		URL:      url,
		FileName: "BuildTools.jar",
		// Jenkins publishes no checksum with the artifact.
		JavaMajor: Java21,
	}, nil
}

// BuildArgs are the arguments BuildTools is run with.
//
// --compile spigot rather than the default, which also builds CraftBukkit and
// the API and takes noticeably longer for artifacts nobody here runs.
// --remapped is not passed: it produces the jars a plugin developer compiles
// against, not the server.
func (s *Spigot) BuildArgs(build *Build) []string {
	return []string{"--rev", build.Version, "--compile", "spigot"}
}

// Produced names the jar BuildTools leaves in the work directory.
func (s *Spigot) Produced(build *Build) string {
	return fmt.Sprintf("spigot-%s.jar", build.Version)
}

// BuildTimeout is how long one compile is given.
//
// BuildTools clones several repositories and compiles Minecraft; twenty
// minutes is comfortable on a small VPS and nowhere near enough to hide a
// hang.
const BuildTimeout = 40 * time.Minute
