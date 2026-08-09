package core

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// GeyserAPIBase is the GeyserMC download API.
const GeyserAPIBase = "https://download.geysermc.org/v2"

// The two projects crossplay needs.
//
// Geyser translates the Bedrock protocol into the Java one. Floodgate is what
// lets those players in without a Java account: without it Geyser connects
// them and the server rejects them for having no Mojang session, which reads
// as "crossplay does not work" rather than as a missing plugin.
const (
	GeyserProject    = "geyser"
	FloodgateProject = "floodgate"
)

// geyserCacheTTL is how long a resolved build is reused.
const geyserCacheTTL = time.Hour

// Crossplay describes the add-ons that make a Java server reachable from a
// Bedrock client.
type Crossplay struct {
	// BaseURL is overridable for tests.
	BaseURL string
	Client  *http.Client

	mu       sync.Mutex
	cached   map[string]*Build
	cachedAt time.Time
	now      func() time.Time
}

// NewCrossplay returns the crossplay add-on resolver.
func NewCrossplay(client *http.Client) *Crossplay {
	return &Crossplay{BaseURL: GeyserAPIBase, Client: client, now: time.Now}
}

// geyserProject lists a project's versions.
type geyserProject struct {
	Versions []string `json:"versions"`
}

// geyserBuild is one build's downloads.
type geyserBuild struct {
	Build     int `json:"build"`
	Downloads map[string]struct {
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
	} `json:"downloads"`
}

// PlatformFor maps a core's loader onto the name GeyserMC publishes under.
//
// Their names are not the loader names: a Paper plugin is published as
// "spigot", and Floodgate spells BungeeCord "bungee" while Geyser spells it
// "bungeecord". Guessing either would 404 at install time, which is why this
// is a table read off their API rather than a transformation.
func PlatformFor(project, loader string) (string, bool) {
	geyser := map[string]string{
		"paper":      "spigot",
		"purpur":     "spigot",
		"spigot":     "spigot",
		"folia":      "spigot",
		"fabric":     "fabric",
		"neoforge":   "neoforge",
		"velocity":   "velocity",
		"bungeecord": "bungeecord",
	}
	floodgate := map[string]string{
		"paper":      "spigot",
		"purpur":     "spigot",
		"spigot":     "spigot",
		"folia":      "spigot",
		"velocity":   "velocity",
		"bungeecord": "bungee",
	}

	table := geyser
	if project == FloodgateProject {
		table = floodgate
	}
	name, ok := table[loader]
	return name, ok
}

// Resolve returns the newest build of a project for a platform.
func (c *Crossplay) Resolve(ctx context.Context, project, platform string) (*Build, error) {
	key := project + "/" + platform

	c.mu.Lock()
	if build, ok := c.cached[key]; ok && c.now().Sub(c.cachedAt) < geyserCacheTTL {
		c.mu.Unlock()
		return build, nil
	}
	c.mu.Unlock()

	var listing geyserProject
	if err := getJSON(ctx, c.Client, c.base()+"/projects/"+project, &listing); err != nil {
		return nil, fmt.Errorf("reading the %s versions: %w", project, err)
	}
	if len(listing.Versions) == 0 {
		return nil, fmt.Errorf("%w: %s publishes no versions", ErrNoBuilds, project)
	}
	// Their listing is oldest first.
	version := listing.Versions[len(listing.Versions)-1]

	var build geyserBuild
	url := fmt.Sprintf("%s/projects/%s/versions/%s/builds/latest", c.base(), project, version)
	if err := getJSON(ctx, c.Client, url, &build); err != nil {
		return nil, fmt.Errorf("reading the latest %s build: %w", project, err)
	}

	download, ok := build.Downloads[platform]
	if !ok {
		return nil, fmt.Errorf("%w: %s publishes no build for %s", ErrNoBuilds, project, platform)
	}

	resolved := &Build{
		Core:    project,
		Version: version,
		Build:   fmt.Sprintf("%d", build.Build),
		URL: fmt.Sprintf("%s/projects/%s/versions/%s/builds/%d/downloads/%s",
			c.base(), project, version, build.Build, platform),
		FileName:  download.Name,
		Checksum:  download.SHA256,
		Algorithm: AlgoSHA256,
	}

	c.mu.Lock()
	if c.cached == nil {
		c.cached = make(map[string]*Build, 2)
	}
	c.cached[key] = resolved
	c.cachedAt = c.now()
	c.mu.Unlock()

	return resolved, nil
}

func (c *Crossplay) base() string {
	if c.BaseURL == "" {
		return GeyserAPIBase
	}
	return c.BaseURL
}

// DefaultBedrockPort is what a Bedrock client tries first, so a server on it
// is found by the client's own LAN discovery without anyone typing a port.
const DefaultBedrockPort = 19132
