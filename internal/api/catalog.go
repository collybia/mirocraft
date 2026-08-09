package api

import (
	"context"
	"crypto/sha1" //nolint:gosec // Modrinth publishes sha1 digests; verifying against them is the point
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/collybia/mirocraft/internal/catalog"
	"github.com/collybia/mirocraft/internal/fileman"
	"github.com/collybia/mirocraft/internal/store"
)

// Limits on catalogue installs.
const (
	// MaxInstallBytes bounds one install, dependencies included. A plugin is
	// a few megabytes; a modpack is not installed through this endpoint.
	MaxInstallBytes = 256 << 20
	// DisabledSuffix marks an add-on that is present but not loaded. The
	// convention is the servers' own: a jar whose name does not end in .jar is
	// ignored, so renaming is how every plugin manager has always done this.
	DisabledSuffix = ".disabled"
	// installTimeout bounds the whole download, dependencies included.
	installTimeout = 10 * time.Minute
)

// Catalog is the add-on registry the API searches. An interface so the tests
// can answer without the network.
type Catalog interface {
	Search(ctx context.Context, opts catalog.SearchOptions) (*catalog.SearchResult, error)
	Project(ctx context.Context, idOrSlug string) (*catalog.Project, error)
	Versions(ctx context.Context, projectID, loader, gameVersion string) ([]catalog.Version, error)
	PlanInstall(ctx context.Context, projectID, versionID, loader, gameVersion string) (*catalog.Plan, error)
}

// --- wire types ---

type installRequest struct {
	ProjectID string `json:"project_id"`
	VersionID string `json:"version_id"`
	// DryRun asks what an install would do without doing it, so the panel can
	// show that one click brings four files before it brings them.
	DryRun bool `json:"dry_run"`
}

type installedAddon struct {
	// File is the name on disk, including the .disabled suffix when it has
	// one: it is the identifier for the delete and toggle endpoints.
	File       string    `json:"file"`
	Name       string    `json:"name"`
	SizeBytes  int64     `json:"size_bytes"`
	Enabled    bool      `json:"enabled"`
	ModifiedAt time.Time `json:"modified_at"`
}

type contentInfo struct {
	// Loader is empty when the core takes no add-ons.
	Loader string `json:"loader"`
	Dir    string `json:"dir"`
	// Version is the Minecraft version add-ons are matched against.
	Version string `json:"version"`
}

// --- helpers ---

// contentFor reports where a server's add-ons live and which loader they must
// be built for.
func (a *API) contentFor(server *store.Server) (contentInfo, error) {
	if a.cores == nil {
		return contentInfo{}, errors.New("the core registry is not configured on this node")
	}
	provider, err := a.cores.Get(server.Core)
	if err != nil {
		return contentInfo{}, err
	}

	content := provider.Content()
	if !content.Accepts() {
		return contentInfo{}, fmt.Errorf(
			"%s does not load plugins or mods; install a core that does, such as Paper", provider.Name())
	}
	return contentInfo{Loader: content.Loader, Dir: content.Dir, Version: server.Version}, nil
}

// --- handlers ---

// handleCatalogSearch serves GET /catalog/search.
func (a *API) handleCatalogSearch(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireScope(w, r, ScopeServersRead); !ok {
		return
	}
	if a.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternalError,
			"the add-on catalogue is not configured on this node")
		return
	}

	query := r.URL.Query()
	limit, _ := strconv.Atoi(query.Get("limit"))
	offset, _ := strconv.Atoi(query.Get("offset"))

	result, err := a.catalog.Search(r.Context(), catalog.SearchOptions{
		Query:       query.Get("q"),
		Type:        query.Get("type"),
		Loader:      query.Get("loader"),
		GameVersion: query.Get("mc"),
		Limit:       limit,
		Offset:      offset,
	})
	if err != nil {
		a.writeCatalogError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleCatalogProject serves GET /catalog/projects/{pid}.
func (a *API) handleCatalogProject(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireScope(w, r, ScopeServersRead); !ok {
		return
	}
	if a.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternalError,
			"the add-on catalogue is not configured on this node")
		return
	}

	project, err := a.catalog.Project(r.Context(), r.PathValue("pid"))
	if err != nil {
		a.writeCatalogError(w, err)
		return
	}

	// Versions are what the panel needs to offer a choice, and asking for them
	// separately would be two round trips for one page.
	versions, err := a.catalog.Versions(r.Context(), project.ID,
		r.URL.Query().Get("loader"), r.URL.Query().Get("mc"))
	if err != nil {
		// The project itself was found; a version lookup that failed should
		// not blank the page.
		a.log.Debug("listing catalogue versions failed", slog.String("error", err.Error()))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"project":  project,
		"versions": versions,
	})
}

// handleServerContent serves GET /servers/{id}/catalog.
//
// The panel needs to know, before it searches, which loader to filter by and
// whether this core takes add-ons at all — otherwise it offers Paper plugins
// for a vanilla server and the install fails at the last step.
func (a *API) handleServerContent(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	if _, ok := a.authorizeServer(w, r, serverID, ScopeServersRead); !ok {
		return
	}

	server, err := a.store.Servers.GetByID(r.Context(), serverID)
	if err != nil {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "server not found")
		return
	}

	content, err := a.contentFor(server)
	if err != nil {
		// Not an error response: "this core takes no add-ons" is an answer,
		// and the panel shows it as an explanation rather than a failure.
		writeJSON(w, http.StatusOK, contentInfo{Version: server.Version})
		return
	}
	writeJSON(w, http.StatusOK, content)
}

// handleCatalogInstall serves POST /servers/{id}/catalog/install.
func (a *API) handleCatalogInstall(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	// files:write rather than servers:write: this writes files into the
	// server's directory, and that is the permission that governs it.
	principal, ok := a.authorizeServer(w, r, serverID, ScopeFilesWrite)
	if !ok {
		return
	}
	if a.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternalError,
			"the add-on catalogue is not configured on this node")
		return
	}

	var req installRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.ProjectID) == "" {
		writeFieldError(w, "project_id", "a project is required")
		return
	}

	server, err := a.store.Servers.GetByID(r.Context(), serverID)
	if err != nil {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "server not found")
		return
	}

	content, err := a.contentFor(server)
	if err != nil {
		writeFieldError(w, "project_id", err.Error())
		return
	}

	plan, err := a.catalog.PlanInstall(r.Context(), req.ProjectID, req.VersionID,
		content.Loader, content.Version)
	if err != nil {
		a.writeCatalogError(w, err)
		return
	}
	if total := plan.TotalBytes(); total > MaxInstallBytes {
		writeFieldError(w, "project_id",
			fmt.Sprintf("this install would download %d MB, more than the %d MB limit",
				total>>20, MaxInstallBytes>>20))
		return
	}

	if req.DryRun {
		writeJSON(w, http.StatusOK, plan)
		return
	}

	// The plan knows exactly how many bytes this will bring, which is the one
	// place a disk allowance can be applied before anything is downloaded.
	if !a.enforceDiskQuota(w, r, server.OwnerID, plan.TotalBytes()) {
		return
	}

	task := a.tasks.start("catalog.install", serverID, principal.UserID,
		func(taskCtx context.Context) error {
			defer a.diskUsage.forget(server.OwnerID)
			return a.runInstall(taskCtx, server, content, plan)
		})

	a.audit(r, principal.UserID, "catalog.install", serverID, req.ProjectID)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"task_id": task.ID,
		"plan":    plan,
	})
}

// runInstall downloads a plan into the server's add-on directory.
func (a *API) runInstall(ctx context.Context, server *store.Server, content contentInfo, plan *catalog.Plan) error {
	ctx, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()

	root, err := fileman.NewRoot(a.serverDir(server))
	if err != nil {
		return fmt.Errorf("opening the server directory: %w", err)
	}
	// fileman.ErrExists, not os.ErrExist: the sandbox reports its own error,
	// and matching the wrong one made every install fail on a server that had
	// ever been started — which is every real one.
	if err := root.Mkdir(content.Dir); err != nil && !errors.Is(err, fileman.ErrExists) {
		return fmt.Errorf("creating %s: %w", content.Dir, err)
	}

	for _, file := range plan.Files {
		// Through the sandbox, not filepath.Join: the file name comes from a
		// third-party registry, and a project publishing an artifact called
		// "../../server.jar" must not be able to overwrite the core.
		target, err := root.Resolve(path.Join(content.Dir, path.Base(file.FileName)))
		if err != nil {
			return fmt.Errorf("refusing the file name %q: %w", file.FileName, err)
		}

		if err := a.downloadAddon(ctx, file, target); err != nil {
			return err
		}
		a.log.Info("add-on installed",
			slog.String("server_id", server.ID), slog.String("project", file.ProjectTitle),
			slog.String("file", file.FileName), slog.Bool("dependency", file.Dependency))
	}
	return nil
}

// downloadAddon fetches one artifact, verifying what upstream published.
func (a *API) downloadAddon(ctx context.Context, file catalog.PlannedFile, target string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, file.URL, nil)
	if err != nil {
		return fmt.Errorf("building the request for %s: %w", file.FileName, err)
	}
	req.Header.Set("User-Agent", catalog.UserAgent)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("downloading %s: %w", file.FileName, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("downloading %s: %s", file.FileName, resp.Status)
	}

	// Written to a temporary file in the same directory and renamed, so an
	// interrupted download cannot leave a truncated jar that the server tries
	// to load on its next start.
	temp, err := os.CreateTemp(filepath.Dir(target), ".mirocraft-*")
	if err != nil {
		return fmt.Errorf("creating a temporary file: %w", err)
	}
	tempName := temp.Name()
	defer func() {
		_ = temp.Close()
		// The name comes from os.CreateTemp, not from the request.
		_ = os.Remove(tempName) // #nosec G703 -- a name this function just created
	}()

	var digest hash.Hash
	switch {
	case file.SHA512 != "":
		digest = sha512.New()
	case file.SHA1 != "":
		digest = sha1.New() //nolint:gosec // matching the digest the index published
	}

	writer := io.Writer(temp)
	if digest != nil {
		writer = io.MultiWriter(temp, digest)
	}

	written, err := io.Copy(writer, io.LimitReader(resp.Body, MaxInstallBytes))
	if err != nil {
		return fmt.Errorf("writing %s: %w", file.FileName, err)
	}
	if file.SizeBytes > 0 && written != file.SizeBytes {
		return fmt.Errorf("%s: got %d bytes, expected %d", file.FileName, written, file.SizeBytes)
	}

	if digest != nil {
		want := file.SHA512
		if want == "" {
			want = file.SHA1
		}
		if got := hex.EncodeToString(digest.Sum(nil)); !strings.EqualFold(got, want) {
			return fmt.Errorf("%s failed its checksum: got %s, expected %s", file.FileName, got, want)
		}
	}

	if err := temp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", file.FileName, err)
	}
	// tempName is this function's own temporary file and target was built and
	// checked by the caller.
	if err := os.Rename(tempName, target); err != nil { // #nosec G703 -- both paths are constructed here
		return fmt.Errorf("installing %s: %w", file.FileName, err)
	}
	return nil
}

// handleListInstalled serves GET /servers/{id}/installed.
func (a *API) handleListInstalled(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	if _, ok := a.authorizeServer(w, r, serverID, ScopeFilesRead); !ok {
		return
	}

	server, err := a.store.Servers.GetByID(r.Context(), serverID)
	if err != nil {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "server not found")
		return
	}

	content, err := a.contentFor(server)
	if err != nil {
		// A core with no add-on directory has nothing installed, which is not
		// an error — it is the empty list.
		writeJSON(w, http.StatusOK, listResponse[installedAddon]{Items: []installedAddon{}})
		return
	}

	root, err := fileman.NewRoot(a.serverDir(server))
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not open the server directory")
		return
	}

	entries, err := root.List(content.Dir)
	if err != nil {
		// No directory yet means nothing installed, which is the ordinary
		// state of a server nobody has added anything to.
		writeJSON(w, http.StatusOK, listResponse[installedAddon]{Items: []installedAddon{}})
		return
	}

	items := make([]installedAddon, 0, len(entries))
	for _, entry := range entries {
		if entry.Type != fileman.TypeFile || !isAddonFile(entry.Name) {
			continue
		}
		enabled := strings.HasSuffix(entry.Name, ".jar")
		items = append(items, installedAddon{
			File:       entry.Name,
			Name:       addonDisplayName(entry.Name),
			SizeBytes:  entry.Size,
			Enabled:    enabled,
			ModifiedAt: entry.ModifiedAt,
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })

	writeJSON(w, http.StatusOK, listResponse[installedAddon]{Items: items})
}

// handleDeleteInstalled serves DELETE /servers/{id}/installed/{file}.
func (a *API) handleDeleteInstalled(w http.ResponseWriter, r *http.Request) {
	server, content, root, ok := a.addonTarget(w, r)
	if !ok {
		return
	}

	name, ok := addonFileName(w, r)
	if !ok {
		return
	}

	if err := root.Remove(path.Join(content.Dir, name)); err != nil {
		a.writeFileError(w, err)
		return
	}

	if principal, found := principalFrom(r.Context()); found {
		a.audit(r, principal.UserID, "catalog.remove", server.ID, name)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleToggleInstalled serves POST /servers/{id}/installed/{file}/toggle.
//
// Renaming rather than moving to a folder: every Bukkit- and Forge-family
// server decides what to load by whether the name ends in .jar, so this is the
// convention operators already know from doing it by hand.
func (a *API) handleToggleInstalled(w http.ResponseWriter, r *http.Request) {
	server, content, root, ok := a.addonTarget(w, r)
	if !ok {
		return
	}

	name, ok := addonFileName(w, r)
	if !ok {
		return
	}

	var renamed string
	enabled := strings.HasSuffix(name, ".jar")
	if enabled {
		renamed = name + DisabledSuffix
	} else {
		renamed = strings.TrimSuffix(name, DisabledSuffix)
	}

	if err := root.Move(path.Join(content.Dir, name), path.Join(content.Dir, renamed)); err != nil {
		a.writeFileError(w, err)
		return
	}

	if principal, found := principalFrom(r.Context()); found {
		a.audit(r, principal.UserID, "catalog.toggle", server.ID, renamed)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"file":    renamed,
		"enabled": !enabled,
		// The server reads its plugin directory once, at startup.
		"restart_required": a.serverIsActive(r.Context(), server.ID),
	})
}

// addonTarget authorizes the request and opens the server's add-on directory.
func (a *API) addonTarget(w http.ResponseWriter, r *http.Request) (*store.Server, contentInfo, *fileman.Root, bool) {
	serverID := r.PathValue("id")
	if _, ok := a.authorizeServer(w, r, serverID, ScopeFilesWrite); !ok {
		return nil, contentInfo{}, nil, false
	}

	server, err := a.store.Servers.GetByID(r.Context(), serverID)
	if err != nil {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "server not found")
		return nil, contentInfo{}, nil, false
	}

	content, err := a.contentFor(server)
	if err != nil {
		writeFieldError(w, "file", err.Error())
		return nil, contentInfo{}, nil, false
	}

	root, err := fileman.NewRoot(a.serverDir(server))
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not open the server directory")
		return nil, contentInfo{}, nil, false
	}
	return server, content, root, true
}

// addonFileName reads the file from the path and checks it is one.
//
// The sandbox would refuse a traversal anyway, but refusing it here means the
// error says "that is not an add-on" rather than something about paths.
func addonFileName(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("file")
	if name == "" || name != path.Base(name) || !isAddonFile(name) {
		writeFieldError(w, "file", "the name must be a jar in the add-on directory")
		return "", false
	}
	return name, true
}

func isAddonFile(name string) bool {
	return strings.HasSuffix(name, ".jar") || strings.HasSuffix(name, ".jar"+DisabledSuffix)
}

// addonDisplayName strips the extension and the disabled marker.
func addonDisplayName(file string) string {
	name := strings.TrimSuffix(file, DisabledSuffix)
	return strings.TrimSuffix(name, ".jar")
}

func (a *API) serverIsActive(ctx context.Context, serverID string) bool {
	status, err := a.serverStatus(ctx, serverID)
	return err == nil && status.IsActive()
}

// writeCatalogError maps an upstream failure onto a response.
func (a *API) writeCatalogError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		writeError(w, http.StatusNotFound, CodeServerNotFound, "project not found")
	case errors.Is(err, catalog.ErrNoVersions):
		writeError(w, http.StatusNotFound, CodeValidationFailed, err.Error())
	case errors.Is(err, catalog.ErrRateLimit):
		// Passed through as 429 rather than dressed up as an internal error:
		// the caller can retry, and telling them so is the whole point.
		writeError(w, http.StatusTooManyRequests, CodeRateLimited,
			"the add-on registry is rate limiting; try again shortly")
	default:
		a.log.Warn("catalogue request failed", slog.String("error", err.Error()))
		writeError(w, http.StatusBadGateway, CodeInternalError,
			"the add-on registry could not be reached")
	}
}
