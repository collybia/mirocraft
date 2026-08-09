package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/collybia/mirocraft/internal/catalog"
	"github.com/collybia/mirocraft/internal/core"
	"github.com/collybia/mirocraft/internal/modpack"
	"github.com/collybia/mirocraft/internal/store"
)

// ModpackRecord is the file an install leaves behind, so the panel can say
// which pack a server is running.
//
// A file in the server directory rather than a column: it belongs to the
// directory it describes, so a restored backup carries the right answer with
// it instead of the database claiming a pack the files no longer hold.
const ModpackRecord = ".mirocraft-modpack.json"

// MaxPackBytes bounds the .mrpack itself — the index and the overrides, not
// the mods it names, which the modpack package bounds separately.
const MaxPackBytes = 256 << 20

// --- wire types ---

type modpackRequest struct {
	ProjectID string `json:"project_id"`
	// VersionID is optional: no version means the newest one published.
	VersionID string `json:"version_id"`
	// DryRun asks what an install would change without changing it.
	DryRun bool `json:"dry_run"`
}

// modpackPlan is what installing a pack would do to this server.
type modpackPlan struct {
	ProjectID string `json:"project_id"`
	VersionID string `json:"version_id"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	File      string `json:"file"`
	SizeBytes int64  `json:"size_bytes"`

	// Core and Minecraft are what the server will be running afterwards.
	Core      string `json:"core"`
	Minecraft string `json:"minecraft"`
	// ChangesCore says the pack needs a different core or Minecraft version
	// from the one the server has, which is the part worth confirming: it
	// replaces what the server runs, not just what it loads.
	ChangesCore bool `json:"changes_core"`
	// ReplacesDir is the directory the install empties first, because the
	// pack's own list of mods is the whole list.
	ReplacesDir string `json:"replaces_dir"`
}

// installedModpack is the record left in the server directory.
type installedModpack struct {
	ProjectID   string    `json:"project_id"`
	VersionID   string    `json:"version_id"`
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	Core        string    `json:"core"`
	Minecraft   string    `json:"minecraft"`
	Files       int       `json:"files"`
	InstalledAt time.Time `json:"installed_at"`
}

// --- handlers ---

// handleGetModpack serves GET /servers/{id}/modpack.
func (a *API) handleGetModpack(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	if _, ok := a.authorizeServer(w, r, serverID, ScopeServersRead); !ok {
		return
	}

	server, err := a.store.Servers.GetByID(r.Context(), serverID)
	if err != nil {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "server not found")
		return
	}

	record, err := readModpackRecord(a.serverDir(server))
	if err != nil {
		// No pack installed is an answer rather than a failure: the panel
		// shows "no modpack" and offers the catalogue.
		writeJSON(w, http.StatusOK, map[string]any{"installed": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"installed": record})
}

// handleInstallModpack serves POST /servers/{id}/modpack.
func (a *API) handleInstallModpack(w http.ResponseWriter, r *http.Request) {
	serverID := r.PathValue("id")
	// servers:write, because this changes the core and the Minecraft version
	// the server runs — not merely what it loads.
	principal, ok := a.authorizeServer(w, r, serverID, ScopeServersWrite)
	if !ok {
		return
	}
	// And files:write, because it empties the mods directory and writes a few
	// hundred files. Two scopes for one call, since it genuinely does both.
	if !principal.HasScope(ScopeFilesWrite) {
		writeError(w, http.StatusForbidden, CodeForbidden,
			"token is missing the "+ScopeFilesWrite+" scope")
		return
	}
	if a.catalog == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternalError,
			"the add-on catalogue is not configured on this node")
		return
	}
	if a.cores == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternalError,
			"the core registry is not configured on this node")
		return
	}

	var req modpackRequest
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
	if err := a.canTakeAModpack(server); err != nil {
		writeFieldError(w, "project_id", err.Error())
		return
	}

	version, file, err := a.resolvePack(r.Context(), req)
	if err != nil {
		if errors.Is(err, errNotAPackVersion) {
			writeFieldError(w, "version_id", err.Error())
			return
		}
		a.writeCatalogError(w, err)
		return
	}

	plan, err := a.planModpack(server, version, file)
	if err != nil {
		writeFieldError(w, "project_id", err.Error())
		return
	}
	if req.DryRun {
		writeJSON(w, http.StatusOK, plan)
		return
	}

	// The install empties the mods directory and replaces the core. A running
	// server has those files open: on Windows the delete fails halfway, on
	// Linux it succeeds and the running process keeps the old jars mapped.
	if status, err := a.serverStatus(r.Context(), serverID); err == nil && status.IsActive() {
		writeError(w, http.StatusConflict, "server_already_running",
			"stop the server before installing a modpack")
		return
	}

	// Charged at the pack's own size, not at what its index will download:
	// what the mods weigh is only knowable after the pack has been read, and
	// by then the request has answered. The allowance still catches the
	// account that installs pack after pack.
	if !a.enforceDiskQuota(w, r, server.OwnerID, plan.SizeBytes) {
		return
	}

	task := a.tasks.startWithProgress("modpack.install", serverID, principal.UserID,
		func(taskCtx context.Context, report func(int)) error {
			defer a.diskUsage.forget(server.OwnerID)
			return a.runModpackInstall(taskCtx, server, plan, file, report)
		})

	a.audit(r, principal.UserID, "modpack.install", serverID, plan.ProjectID+"@"+plan.VersionID)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"task_id": task.ID,
		"plan":    plan,
	})
}

// --- the work ---

// errNotAPackVersion reports a version that is not a modpack.
var errNotAPackVersion = errors.New("this version is not a modpack")

// canTakeAModpack refuses the servers a modpack cannot go on.
//
// A Modrinth modpack is a set of Java mods and a loader. Installing one on a
// Bedrock server or a proxy would replace what the server runs with something
// unrelated to what the operator asked for.
func (a *API) canTakeAModpack(server *store.Server) error {
	provider, err := a.cores.Get(server.Core)
	if err != nil {
		return err
	}
	if provider.Kind() != core.KindServer || provider.Runtime() != core.RuntimeJava {
		return fmt.Errorf("%s takes no modpacks: a modpack is a set of Java mods and a loader",
			provider.Name())
	}
	return nil
}

// resolvePack finds the version to install and the file to download.
func (a *API) resolvePack(ctx context.Context, req modpackRequest) (*catalog.Version, catalog.File, error) {
	versions, err := a.catalog.Versions(ctx, req.ProjectID, "", "")
	if err != nil {
		return nil, catalog.File{}, err
	}
	if len(versions) == 0 {
		return nil, catalog.File{}, catalog.ErrNoVersions
	}

	// Newest first, so no version asked for means the newest published.
	chosen := &versions[0]
	if req.VersionID != "" {
		chosen = nil
		for i := range versions {
			if versions[i].ID == req.VersionID {
				chosen = &versions[i]
				break
			}
		}
		if chosen == nil {
			return nil, catalog.File{}, fmt.Errorf("%w: %s", catalog.ErrNotFound, req.VersionID)
		}
	}

	file, ok := chosen.PrimaryFile()
	if !ok {
		return nil, catalog.File{}, fmt.Errorf("%w: it has no files", errNotAPackVersion)
	}
	// The extension is the check that this is a pack at all: a mod's version
	// looks identical up to here, and installing a jar as a pack would fail
	// later with a message about a zip.
	if !strings.HasSuffix(strings.ToLower(file.Name), ".mrpack") {
		return nil, catalog.File{}, fmt.Errorf("%w: %s is not a .mrpack", errNotAPackVersion, file.Name)
	}
	if file.Size > MaxPackBytes {
		return nil, catalog.File{}, fmt.Errorf("%w: %s is %d MB", errNotAPackVersion, file.Name, file.Size>>20)
	}
	return chosen, file, nil
}

// planModpack says what installing this version would change.
func (a *API) planModpack(server *store.Server, version *catalog.Version, file catalog.File) (*modpackPlan, error) {
	plan := &modpackPlan{
		ProjectID: version.ProjectID,
		VersionID: version.ID,
		Name:      version.Name,
		Version:   version.Number,
		File:      file.Name,
		SizeBytes: file.Size,
	}

	// Read off the version listing rather than the pack, so the panel can show
	// this before anything is downloaded. The index decides the install.
	for _, loader := range version.Loaders {
		if id, ok := modpack.CoreForLoader(loader); ok {
			plan.Core = id
			break
		}
	}
	if plan.Core == "" {
		return nil, fmt.Errorf("this pack needs %s, which the panel does not install",
			strings.Join(version.Loaders, ", "))
	}
	if len(version.GameVersions) > 0 {
		plan.Minecraft = version.GameVersions[0]
	}

	provider, err := a.cores.Get(plan.Core)
	if err != nil {
		return nil, err
	}
	plan.ReplacesDir = provider.Content().Dir
	plan.ChangesCore = server.Core != plan.Core ||
		(plan.Minecraft != "" && server.Version != plan.Minecraft)
	return plan, nil
}

// runModpackInstall downloads a pack and puts the server on it.
func (a *API) runModpackInstall(
	ctx context.Context, server *store.Server, plan *modpackPlan, file catalog.File, report func(int),
) error {
	dir := a.serverDir(server)
	if dir == "" {
		return errors.New("the server has no directory")
	}

	// Beside the server directories rather than in the system temp: a pack is
	// a few hundred megabytes and /tmp is a small tmpfs on a lot of hosts.
	work, err := os.MkdirTemp(a.dataDir, ".modpack-")
	if err != nil {
		return fmt.Errorf("creating a working directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(work) }()

	packPath := filepath.Join(work, "pack.mrpack")
	if err := a.downloadAddon(ctx, catalog.PlannedFile{
		FileName: file.Name, URL: file.URL, SizeBytes: file.Size,
		SHA512: file.SHA512, SHA1: file.SHA1,
	}, packPath); err != nil {
		return err
	}
	report(5)

	index, err := modpack.ReadIndex(packPath)
	if err != nil {
		return err
	}
	// The index, not the version listing: what the pack says it needs is what
	// it needs, and the two disagree often enough for packs published under a
	// loader tag that does not match their dependencies.
	loader, err := index.LoaderFor()
	if err != nil {
		return err
	}
	provider, err := a.cores.Get(loader.Core)
	if err != nil {
		return fmt.Errorf("this pack needs %s: %w", loader.Core, err)
	}

	// The pack's list of mods is the whole list. Anything left from before
	// would be loaded alongside it, which is how a modpack crashes on start
	// with a duplicate-mod error nobody can trace back to this moment.
	// content.Dir is the core's own constant — "mods" — and dir is derived from
	// the data directory, so neither comes from the request.
	if content := provider.Content(); content.Accepts() {
		if err := os.RemoveAll(filepath.Join(dir, content.Dir)); err != nil { // #nosec G703 -- both parts are the panel's own
			return fmt.Errorf("clearing %s: %w", content.Dir, err)
		}
	}

	installer := &modpack.Installer{Log: a.log}
	// 5..90 is the download of the files the index names; the rest is
	// installing the loader, which is one download and sometimes a run.
	installed, err := installer.Install(ctx, packPath, dir, func(done, total int) {
		if total > 0 {
			report(5 + done*85/total)
		}
	})
	if err != nil {
		return err
	}
	report(90)

	a.log.Info("modpack installed",
		slog.String("server_id", server.ID), slog.String("pack", installed.Name),
		slog.String("core", loader.Core), slog.String("minecraft", loader.Minecraft),
		slog.Int("files", len(installed.Files)))

	// Recorded before provisioning: the files are on disk either way, and a
	// loader that fails to download should not leave the panel claiming the
	// server has no pack when it plainly does.
	record := installedModpack{
		ProjectID: plan.ProjectID, VersionID: plan.VersionID,
		Name: displayName(installed.Name, plan.Name), Version: plan.Version,
		Core: loader.Core, Minecraft: loader.Minecraft,
		Files: len(installed.Files), InstalledAt: time.Now().UTC(),
	}
	if err := writeModpackRecord(dir, record); err != nil {
		a.log.Warn("recording the installed modpack failed",
			slog.String("server_id", server.ID), slog.String("error", err.Error()))
	}

	// The core and the version the pack asks for, so the next start installs
	// the loader the mods were built against. JarName is cleared because the
	// jar that is about to be installed is a different one.
	server.Core = loader.Core
	server.Version = loader.Minecraft
	server.JarName = ""
	if err := a.store.Servers.Update(ctx, server); err != nil {
		return fmt.Errorf("switching the server to %s %s: %w", loader.Core, loader.Minecraft, err)
	}

	// Installing the loader here rather than leaving it to the next start, so
	// that a pack needing a loader the panel cannot build fails now, while the
	// operator is watching, rather than on a start hours later.
	if a.provisioner != nil {
		if _, err := a.provisioner.Prepare(ctx, server, dir); err != nil {
			return fmt.Errorf("installing %s for %s: %w", loader.Core, loader.Minecraft, err)
		}
	}
	return nil
}

// displayName prefers the name the pack calls itself.
func displayName(fromIndex, fromCatalog string) string {
	if strings.TrimSpace(fromIndex) != "" {
		return fromIndex
	}
	return fromCatalog
}

// --- the record ---

func writeModpackRecord(dir string, record installedModpack) error {
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	// A fixed name under a directory the panel derived itself.
	return os.WriteFile(filepath.Join(dir, ModpackRecord), body, 0o640) // #nosec G703 -- fixed name under the server directory
}

func readModpackRecord(dir string) (*installedModpack, error) {
	if dir == "" {
		return nil, errors.New("the server has no directory")
	}
	// A path built from the data directory and a constant.
	body, err := os.ReadFile(filepath.Join(dir, ModpackRecord)) // #nosec G304,G703 -- fixed name under the server directory
	if err != nil {
		return nil, err
	}

	var record installedModpack
	if err := json.Unmarshal(body, &record); err != nil {
		return nil, err
	}
	return &record, nil
}
