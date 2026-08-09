// Package catalog searches and installs add-ons — plugins, mods and modpacks —
// from Modrinth.
//
// Modrinth rather than a scraper over SpigotMC or CurseForge because it is the
// only one of the three with a documented, unauthenticated API and permissive
// download URLs. CurseForge needs a key per deployment, which a self-hosted
// panel cannot ship, and SpigotMC has no API at all.
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultBaseURL is Modrinth's API.
const DefaultBaseURL = "https://api.modrinth.com/v2"

// UserAgent identifies this panel to Modrinth.
//
// Their guidelines ask for a contactable agent, and an anonymous client is the
// first thing rate-limited when someone abuses the API. Version is stamped by
// the daemon at startup.
var UserAgent = "mirocraft/dev (+https://github.com/collybia/mirocraft)"

// Errors callers distinguish.
var (
	ErrNotFound   = errors.New("catalog: project not found")
	ErrNoVersions = errors.New("catalog: nothing published for this loader and Minecraft version")
	ErrRateLimit  = errors.New("catalog: upstream is rate limiting")
)

// Project is an add-on as the catalogue lists it.
//
// Deliberately not the whole upstream document: the panel shows a card and a
// detail page, and passing through fields nobody reads would tie this to
// Modrinth's schema more tightly than necessary.
type Project struct {
	ID          string   `json:"id"`
	Slug        string   `json:"slug"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Type        string   `json:"type"`
	Downloads   int      `json:"downloads"`
	IconURL     string   `json:"icon_url,omitempty"`
	Categories  []string `json:"categories,omitempty"`
	Loaders     []string `json:"loaders,omitempty"`
	License     string   `json:"license,omitempty"`

	// ServerSide is required, optional or unsupported. A "client" mod on a
	// server does nothing, and the panel says so rather than installing it.
	ServerSide string `json:"server_side,omitempty"`
	ClientSide string `json:"client_side,omitempty"`

	// Body is the long description, filled only by a detail lookup.
	Body string `json:"body,omitempty"`
}

// SearchResult is one page of hits.
type SearchResult struct {
	Items  []Project `json:"items"`
	Total  int       `json:"total"`
	Offset int       `json:"offset"`
}

// File is one downloadable artifact of a version.
type File struct {
	Name    string `json:"name"`
	URL     string `json:"url"`
	Size    int64  `json:"size"`
	Primary bool   `json:"primary"`
	SHA512  string `json:"sha512,omitempty"`
	SHA1    string `json:"sha1,omitempty"`
}

// Dependency is another project a version needs.
type Dependency struct {
	ProjectID string `json:"project_id,omitempty"`
	VersionID string `json:"version_id,omitempty"`
	Type      string `json:"type"` // required | optional | incompatible | embedded
}

// Version is one release of a project.
type Version struct {
	ID           string       `json:"id"`
	ProjectID    string       `json:"project_id"`
	Name         string       `json:"name"`
	Number       string       `json:"number"`
	Channel      string       `json:"channel"` // release | beta | alpha
	Loaders      []string     `json:"loaders"`
	GameVersions []string     `json:"game_versions"`
	PublishedAt  time.Time    `json:"published_at,omitzero"`
	Files        []File       `json:"files"`
	Dependencies []Dependency `json:"dependencies,omitempty"`
}

// PrimaryFile returns the file to install.
//
// A version can carry sources or javadoc alongside the artifact; Modrinth
// marks the real one primary. Falling back to the first file rather than
// failing, because a few older versions predate the flag.
func (v *Version) PrimaryFile() (File, bool) {
	for _, file := range v.Files {
		if file.Primary {
			return file, true
		}
	}
	if len(v.Files) > 0 {
		return v.Files[0], true
	}
	return File{}, false
}

// Client talks to Modrinth.
type Client struct {
	BaseURL string
	HTTP    *http.Client

	cache *cache
}

// New returns a client with a cache in front of it.
func New(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}
	return &Client{
		BaseURL: DefaultBaseURL,
		HTTP:    httpClient,
		cache:   newCache(),
	}
}

// SearchOptions narrows a search.
type SearchOptions struct {
	Query string
	// Type filters by project type: mod, modpack, resourcepack, datapack,
	// shader. Empty means any.
	Type string
	// Loader is the loader id — paper, fabric, forge and so on. Modrinth
	// carries it as a category on search, not as a separate facet.
	Loader string
	// GameVersion is the Minecraft version.
	GameVersion string

	Limit  int
	Offset int
}

// Search returns matching projects.
func (c *Client) Search(ctx context.Context, opts SearchOptions) (*SearchResult, error) {
	limit := opts.Limit
	if limit <= 0 || limit > 50 {
		limit = 20
	}

	// Facets are AND across the outer array and OR within each inner one.
	var facets [][]string
	if opts.Type != "" {
		facets = append(facets, []string{"project_type:" + opts.Type})
	}
	if opts.Loader != "" {
		facets = append(facets, []string{"categories:" + opts.Loader})
	}
	if opts.GameVersion != "" {
		facets = append(facets, []string{"versions:" + opts.GameVersion})
	}
	// A server has no screen. Modrinth marks purely client-side projects as
	// server_side: unsupported, and installing one does nothing at all, so
	// they are filtered out rather than offered and then quietly ignored.
	facets = append(facets, []string{"server_side:required", "server_side:optional"})

	query := url.Values{
		"query":  {opts.Query},
		"limit":  {strconv.Itoa(limit)},
		"offset": {strconv.Itoa(opts.Offset)},
		"index":  {"relevance"},
	}
	if len(facets) > 0 {
		encoded, err := json.Marshal(facets)
		if err != nil {
			return nil, err
		}
		query.Set("facets", string(encoded))
	}

	key := "search:" + query.Encode()
	if cached, ok := c.cache.get(key); ok {
		// The cache is keyed by a prefix this method owns, so nothing else
		// can have stored another type under it; a mismatch would be a bug
		// here rather than bad input, and the zero value would hide it.
		if result, ok := cached.(*SearchResult); ok {
			return result, nil
		}
	}

	var raw struct {
		Hits []struct {
			ProjectID   string   `json:"project_id"`
			Slug        string   `json:"slug"`
			Title       string   `json:"title"`
			Description string   `json:"description"`
			ProjectType string   `json:"project_type"`
			Downloads   int      `json:"downloads"`
			IconURL     string   `json:"icon_url"`
			Categories  []string `json:"categories"`
			License     string   `json:"license"`
			ServerSide  string   `json:"server_side"`
			ClientSide  string   `json:"client_side"`
		} `json:"hits"`
		Offset    int `json:"offset"`
		TotalHits int `json:"total_hits"`
	}
	if err := c.get(ctx, "/search", query, &raw); err != nil {
		return nil, err
	}

	result := &SearchResult{Total: raw.TotalHits, Offset: raw.Offset}
	for _, hit := range raw.Hits {
		result.Items = append(result.Items, Project{
			ID: hit.ProjectID, Slug: hit.Slug, Title: hit.Title,
			Description: hit.Description, Type: hit.ProjectType,
			Downloads: hit.Downloads, IconURL: hit.IconURL,
			Categories: hit.Categories, License: hit.License,
			ServerSide: hit.ServerSide, ClientSide: hit.ClientSide,
		})
	}

	c.cache.set(key, result, searchTTL)
	return result, nil
}

// Project returns one project by id or slug.
func (c *Client) Project(ctx context.Context, idOrSlug string) (*Project, error) {
	key := "project:" + idOrSlug
	if cached, ok := c.cache.get(key); ok {
		if project, ok := cached.(*Project); ok {
			return project, nil
		}
	}

	var raw struct {
		ID          string   `json:"id"`
		Slug        string   `json:"slug"`
		Title       string   `json:"title"`
		Description string   `json:"description"`
		Body        string   `json:"body"`
		ProjectType string   `json:"project_type"`
		Downloads   int      `json:"downloads"`
		IconURL     string   `json:"icon_url"`
		Categories  []string `json:"categories"`
		Loaders     []string `json:"loaders"`
		ServerSide  string   `json:"server_side"`
		ClientSide  string   `json:"client_side"`
		License     struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"license"`
	}
	if err := c.get(ctx, "/project/"+url.PathEscape(idOrSlug), nil, &raw); err != nil {
		return nil, err
	}

	project := &Project{
		ID: raw.ID, Slug: raw.Slug, Title: raw.Title,
		Description: raw.Description, Body: raw.Body, Type: raw.ProjectType,
		Downloads: raw.Downloads, IconURL: raw.IconURL,
		Categories: raw.Categories, Loaders: raw.Loaders,
		License: raw.License.ID, ServerSide: raw.ServerSide, ClientSide: raw.ClientSide,
	}
	c.cache.set(key, project, projectTTL)
	return project, nil
}

// Versions lists a project's versions for a loader and Minecraft version,
// newest first. Empty filters mean no filtering.
func (c *Client) Versions(ctx context.Context, projectID, loader, gameVersion string) ([]Version, error) {
	query := url.Values{}
	if loader != "" {
		query.Set("loaders", `["`+loader+`"]`)
	}
	if gameVersion != "" {
		query.Set("game_versions", `["`+gameVersion+`"]`)
	}

	key := "versions:" + projectID + ":" + query.Encode()
	if cached, ok := c.cache.get(key); ok {
		if versions, ok := cached.([]Version); ok {
			return versions, nil
		}
	}

	var raw []versionPayload
	if err := c.get(ctx, "/project/"+url.PathEscape(projectID)+"/version", query, &raw); err != nil {
		return nil, err
	}

	versions := make([]Version, 0, len(raw))
	for _, item := range raw {
		versions = append(versions, item.toVersion())
	}

	c.cache.set(key, versions, versionsTTL)
	return versions, nil
}

// VersionByID returns one version.
func (c *Client) VersionByID(ctx context.Context, versionID string) (*Version, error) {
	key := "version:" + versionID
	if cached, ok := c.cache.get(key); ok {
		if version, ok := cached.(*Version); ok {
			return version, nil
		}
	}

	var raw versionPayload
	if err := c.get(ctx, "/version/"+url.PathEscape(versionID), nil, &raw); err != nil {
		return nil, err
	}

	version := raw.toVersion()
	c.cache.set(key, &version, versionsTTL)
	return &version, nil
}

// versionPayload is the upstream shape, converted rather than exposed.
type versionPayload struct {
	ID            string    `json:"id"`
	ProjectID     string    `json:"project_id"`
	Name          string    `json:"name"`
	VersionNumber string    `json:"version_number"`
	VersionType   string    `json:"version_type"`
	Loaders       []string  `json:"loaders"`
	GameVersions  []string  `json:"game_versions"`
	DatePublished time.Time `json:"date_published"`
	Files         []struct {
		Filename string            `json:"filename"`
		URL      string            `json:"url"`
		Size     int64             `json:"size"`
		Primary  bool              `json:"primary"`
		Hashes   map[string]string `json:"hashes"`
	} `json:"files"`
	Dependencies []struct {
		ProjectID      string `json:"project_id"`
		VersionID      string `json:"version_id"`
		DependencyType string `json:"dependency_type"`
	} `json:"dependencies"`
}

func (p versionPayload) toVersion() Version {
	version := Version{
		ID: p.ID, ProjectID: p.ProjectID, Name: p.Name, Number: p.VersionNumber,
		Channel: p.VersionType, Loaders: p.Loaders, GameVersions: p.GameVersions,
		PublishedAt: p.DatePublished,
	}
	for _, file := range p.Files {
		version.Files = append(version.Files, File{
			Name: file.Filename, URL: file.URL, Size: file.Size, Primary: file.Primary,
			SHA512: file.Hashes["sha512"], SHA1: file.Hashes["sha1"],
		})
	}
	for _, dep := range p.Dependencies {
		version.Dependencies = append(version.Dependencies, Dependency{
			ProjectID: dep.ProjectID, VersionID: dep.VersionID, Type: dep.DependencyType,
		})
	}
	return version
}

// get performs a request and decodes the response.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	target := base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("catalog: building the request: %w", err)
	}
	req.Header.Set("User-Agent", UserAgent)
	req.Header.Set("Accept", "application/json")

	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("catalog: %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, strings.TrimPrefix(path, "/"))
	case resp.StatusCode == http.StatusTooManyRequests:
		// Worth naming: an operator seeing "rate limited" knows to wait,
		// where "500" sends them looking for a bug in the panel.
		return ErrRateLimit
	case resp.StatusCode >= 400:
		preview, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return fmt.Errorf("catalog: %s: %s: %s", path, resp.Status, strings.TrimSpace(string(preview)))
	}

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("catalog: decoding %s: %w", path, err)
	}
	return nil
}
