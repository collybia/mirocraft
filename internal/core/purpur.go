package core

import (
	"context"
	"fmt"
	"net/http"
	"sort"
)

// PurpurAPIBase is the PurpurMC API root.
const PurpurAPIBase = "https://api.purpurmc.org/v2"

// Purpur serves PurpurMC builds.
//
// Purpur is a fork of Paper, so its plugins are Paper's — but the add-on
// registries list it as a loader of its own, and a plugin published only for
// Purpur exists. The loader is therefore "purpur" rather than "paper".
type Purpur struct {
	// BaseURL is overridable for tests.
	BaseURL string
	Client  *http.Client
}

// NewPurpur returns the Purpur provider.
func NewPurpur(client *http.Client) *Purpur {
	return &Purpur{BaseURL: PurpurAPIBase, Client: client}
}

// ID returns the identifier the API and the database use for this core.
func (p *Purpur) ID() string { return "purpur" }

// Name returns the name shown in the panel.
func (p *Purpur) Name() string { return "Purpur" }

// Kind reports that this core is a server rather than a proxy.
func (p *Purpur) Kind() Kind { return KindServer }

// Runtime reports what has to be installed to run this core.
func (p *Purpur) Runtime() Runtime { return RuntimeJava }

// Content reports where this core's add-ons go.
func (p *Purpur) Content() Content {
	return Content{Loader: "purpur", Dir: "plugins"}
}

// purpurProject is the project document: {"versions": ["1.14.1", ...]}.
type purpurProject struct {
	Project  string   `json:"project"`
	Versions []string `json:"versions"`
}

// purpurVersion lists a version's builds.
type purpurVersion struct {
	Builds struct {
		Latest string   `json:"latest"`
		All    []string `json:"all"`
	} `json:"builds"`
}

// purpurBuild is one build's detail. The md5 is what makes a download
// verifiable; Purpur publishes nothing stronger.
type purpurBuild struct {
	Build  string `json:"build"`
	Result string `json:"result"`
	MD5    string `json:"md5"`
}

// Versions lists what Purpur publishes, newest first.
func (p *Purpur) Versions(ctx context.Context) ([]Version, error) {
	var project purpurProject
	if err := getJSON(ctx, p.Client, p.base()+"/purpur", &project); err != nil {
		return nil, fmt.Errorf("reading the purpur project: %w", err)
	}

	out := make([]Version, 0, len(project.Versions))
	for _, id := range project.Versions {
		channel := ChannelSnapshot
		if IsRelease(id) {
			channel = ChannelRelease
		}
		out = append(out, Version{ID: id, Channel: channel, JavaMajor: JavaMajorFor(id)})
	}

	// Purpur returns them oldest first; the panel shows newest first.
	sort.SliceStable(out, func(i, j int) bool {
		return CompareVersions(out[i].ID, out[j].ID) > 0
	})
	return out, nil
}

// Resolve picks the latest successful build for a version.
func (p *Purpur) Resolve(ctx context.Context, version string) (*Build, error) {
	if version == "" {
		versions, err := p.Versions(ctx)
		if err != nil {
			return nil, err
		}
		if len(versions) == 0 {
			return nil, fmt.Errorf("%w: purpur publishes no versions", ErrUnknownVersion)
		}
		version = newestRelease(versions)
	}

	var builds purpurVersion
	if err := getJSON(ctx, p.Client, p.base()+"/purpur/"+version, &builds); err != nil {
		return nil, fmt.Errorf("%w: purpur %s", ErrUnknownVersion, version)
	}
	if builds.Builds.Latest == "" {
		return nil, fmt.Errorf("%w: purpur %s", ErrNoBuilds, version)
	}

	var detail purpurBuild
	url := fmt.Sprintf("%s/purpur/%s/%s", p.base(), version, builds.Builds.Latest)
	if err := getJSON(ctx, p.Client, url, &detail); err != nil {
		return nil, fmt.Errorf("reading purpur build %s of %s: %w", builds.Builds.Latest, version, err)
	}
	// A build that did not compile is still listed. Running it would fail in a
	// way that looks like a broken panel rather than a broken build.
	if detail.Result != "" && detail.Result != "SUCCESS" {
		return nil, fmt.Errorf("%w: the latest purpur build for %s is %s",
			ErrNoBuilds, version, detail.Result)
	}

	return &Build{
		Core:      p.ID(),
		Version:   version,
		Build:     builds.Builds.Latest,
		URL:       fmt.Sprintf("%s/purpur/%s/%s/download", p.base(), version, builds.Builds.Latest),
		FileName:  fmt.Sprintf("purpur-%s-%s.jar", version, builds.Builds.Latest),
		Checksum:  detail.MD5,
		Algorithm: AlgoMD5,
		JavaMajor: JavaMajorFor(version),
	}, nil
}

func (p *Purpur) base() string {
	if p.BaseURL == "" {
		return PurpurAPIBase
	}
	return p.BaseURL
}

// newestRelease returns the newest release from a sorted version list, falling
// back to the newest of anything when a project has only pre-releases.
//
// Shared by the providers that have to answer "latest" themselves: a release
// candidate sorts high and is not what somebody asking for the latest wants to
// put players on.
func newestRelease(versions []Version) string {
	for _, v := range versions {
		if v.Channel == ChannelRelease {
			return v.ID
		}
	}
	if len(versions) > 0 {
		return versions[0].ID
	}
	return ""
}
