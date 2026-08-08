package api

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/collybia/mirocraft/internal/gamefiles"
	"github.com/collybia/mirocraft/internal/mcping"
	"github.com/collybia/mirocraft/internal/runner"
	"github.com/collybia/mirocraft/internal/store"
)

// MaxReasonLength caps a kick or ban reason. It ends up in a console command,
// so it is bounded as well as sanitised.
const MaxReasonLength = 200

// --- wire types ---

type onlinePlayersResponse struct {
	// Online is what the server reports.
	Online int `json:"online"`
	Max    int `json:"max"`
	// Items is the sample the protocol offers, which is capped by the server
	// and suppressed entirely when hide-online-players is on. Named a sample
	// rather than a list because it is not guaranteed to be complete.
	Items    []mcping.Player `json:"items"`
	Complete bool            `json:"complete"`
}

type playerActionRequest struct {
	Reason string `json:"reason"`
}

type playerNameRequest struct {
	Name string `json:"name"`
}

type whitelistStateRequest struct {
	Enabled bool `json:"enabled"`
}

type whitelistResponse struct {
	Enabled bool               `json:"enabled"`
	Items   []gamefiles.Player `json:"items"`
}

type settingsResponse struct {
	Values map[string]string   `json:"values"`
	Schema []gamefiles.Setting `json:"schema"`
	// Managed keys the panel owns; the panel shows them read-only.
	Managed map[string]string `json:"managed"`
}

type patchSettingsRequest map[string]string

// --- helpers ---

// gameDir authorizes the request and returns the server's directory.
func (a *API) gameDir(w http.ResponseWriter, r *http.Request, scope string) (*store.Server, string, bool) {
	serverID := r.PathValue("id")
	if _, ok := a.authorizeServer(w, r, serverID, scope); !ok {
		return nil, "", false
	}

	server, err := a.store.Servers.GetByID(r.Context(), serverID)
	if err != nil {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "server not found")
		return nil, "", false
	}
	return server, a.serverDir(server), true
}

// pathPlayerName reads and validates the player name in the path.
func pathPlayerName(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if err := gamefiles.ValidatePlayerName(name); err != nil {
		writeFieldError(w, "name", err.Error())
		return "", false
	}
	return name, true
}

// sanitizeReason makes an operator's free text safe to put in a console
// command.
//
// The command is a single line written to stdin, so a newline would inject a
// second command with the panel's authority. Stripping rather than refusing
// keeps a pasted multi-line reason usable.
func sanitizeReason(reason string) string {
	replacer := strings.NewReplacer("\n", " ", "\r", " ", "\t", " ")
	clean := strings.TrimSpace(replacer.Replace(reason))

	if len([]rune(clean)) > MaxReasonLength {
		runes := []rune(clean)
		clean = string(runes[:MaxReasonLength])
	}
	return clean
}

// sendGameCommand runs a console command against a running server.
func (a *API) sendGameCommand(ctx context.Context, serverID, command string) error {
	if a.console == nil {
		return runner.ErrRunnerUnavailable
	}
	return a.console.SendCommand(ctx, serverID, command)
}

// requireRunning writes the error response for actions that need a live
// server and reports whether to continue.
func (a *API) requireRunning(w http.ResponseWriter, r *http.Request, serverID string) bool {
	status, err := a.serverStatus(r.Context(), serverID)
	if err != nil || !status.IsActive() {
		writeError(w, http.StatusConflict, CodeServerNotRunning,
			"the server must be running for this")
		return false
	}
	return true
}

// --- players ---

// handleListPlayers serves GET /servers/{id}/players.
func (a *API) handleListPlayers(w http.ResponseWriter, r *http.Request) {
	server, _, ok := a.gameDir(w, r, ScopeServersRead)
	if !ok {
		return
	}

	resp := onlinePlayersResponse{Items: []mcping.Player{}}

	status, err := a.serverStatus(r.Context(), server.ID)
	if err != nil || !status.IsActive() {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	ping := a.ping
	if ping == nil {
		ping = mcping.Ping
	}
	pingCtx, cancel := context.WithTimeout(r.Context(), mcping.DefaultTimeout)
	defer cancel()

	pong, err := ping(pingCtx, "127.0.0.1", server.Port)
	if err != nil {
		// A server that is up but not yet accepting connections is a normal
		// state during startup, not an error.
		writeJSON(w, http.StatusOK, resp)
		return
	}

	resp.Online = pong.PlayersOnline
	resp.Max = pong.PlayersMax
	if pong.Sample != nil {
		resp.Items = pong.Sample
	}
	// The sample is complete only when it accounts for everyone online.
	resp.Complete = len(resp.Items) == resp.Online

	writeJSON(w, http.StatusOK, resp)
}

// handleKickPlayer serves POST /servers/{id}/players/{name}/kick.
func (a *API) handleKickPlayer(w http.ResponseWriter, r *http.Request) {
	server, _, ok := a.gameDir(w, r, ScopeServersWrite)
	if !ok {
		return
	}
	name, ok := pathPlayerName(w, r)
	if !ok {
		return
	}

	var req playerActionRequest
	if !decodeBody(w, r, &req) {
		return
	}
	// Kicking only makes sense against a live server: there is nobody to kick
	// from a stopped one.
	if !a.requireRunning(w, r, server.ID) {
		return
	}

	command := "kick " + name
	if reason := sanitizeReason(req.Reason); reason != "" {
		command += " " + reason
	}

	if err := a.sendGameCommand(r.Context(), server.ID, command); err != nil {
		a.writeRunnerError(w, server.ID, err)
		return
	}

	a.auditGame(r, "player.kick", name)
	w.WriteHeader(http.StatusNoContent)
}

// handleBanPlayer serves POST /servers/{id}/players/{name}/ban.
func (a *API) handleBanPlayer(w http.ResponseWriter, r *http.Request) {
	server, _, ok := a.gameDir(w, r, ScopeServersWrite)
	if !ok {
		return
	}
	name, ok := pathPlayerName(w, r)
	if !ok {
		return
	}

	var req playerActionRequest
	if !decodeBody(w, r, &req) {
		return
	}
	// Banning does work on a stopped server in principle, but only the
	// running server writes banned-players.json, so it is refused rather
	// than silently doing nothing.
	if !a.requireRunning(w, r, server.ID) {
		return
	}

	command := "ban " + name
	if reason := sanitizeReason(req.Reason); reason != "" {
		command += " " + reason
	}

	if err := a.sendGameCommand(r.Context(), server.ID, command); err != nil {
		a.writeRunnerError(w, server.ID, err)
		return
	}

	a.auditGame(r, "player.ban", name)
	w.WriteHeader(http.StatusNoContent)
}

// handleUnbanPlayer serves DELETE /servers/{id}/players/{name}/ban.
func (a *API) handleUnbanPlayer(w http.ResponseWriter, r *http.Request) {
	server, _, ok := a.gameDir(w, r, ScopeServersWrite)
	if !ok {
		return
	}
	name, ok := pathPlayerName(w, r)
	if !ok {
		return
	}
	if !a.requireRunning(w, r, server.ID) {
		return
	}

	if err := a.sendGameCommand(r.Context(), server.ID, "pardon "+name); err != nil {
		a.writeRunnerError(w, server.ID, err)
		return
	}

	a.auditGame(r, "player.unban", name)
	w.WriteHeader(http.StatusNoContent)
}

// handleListBans serves GET /servers/{id}/bans.
func (a *API) handleListBans(w http.ResponseWriter, r *http.Request) {
	_, dir, ok := a.gameDir(w, r, ScopeServersRead)
	if !ok {
		return
	}

	bans, err := gamefiles.LoadBans(dir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not read the ban list")
		return
	}
	if bans == nil {
		bans = []gamefiles.Ban{}
	}
	writeJSON(w, http.StatusOK, listResponse[gamefiles.Ban]{Items: bans})
}

// --- whitelist ---

// handleGetWhitelist serves GET /servers/{id}/whitelist.
func (a *API) handleGetWhitelist(w http.ResponseWriter, r *http.Request) {
	_, dir, ok := a.gameDir(w, r, ScopeServersRead)
	if !ok {
		return
	}

	players, err := gamefiles.LoadWhitelist(dir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not read the whitelist")
		return
	}
	if players == nil {
		players = []gamefiles.Player{}
	}
	sort.Slice(players, func(i, j int) bool {
		return strings.ToLower(players[i].Name) < strings.ToLower(players[j].Name)
	})

	enabled := false
	if props, err := gamefiles.LoadProperties(dir); err == nil {
		if v, ok := props.Get("white-list"); ok {
			enabled = v == "true"
		}
	}

	writeJSON(w, http.StatusOK, whitelistResponse{Enabled: enabled, Items: players})
}

// handleAddToWhitelist serves POST /servers/{id}/whitelist.
func (a *API) handleAddToWhitelist(w http.ResponseWriter, r *http.Request) {
	a.whitelistChange(w, r, "add")
}

// handleRemoveFromWhitelist serves DELETE /servers/{id}/whitelist/{name}.
func (a *API) handleRemoveFromWhitelist(w http.ResponseWriter, r *http.Request) {
	a.whitelistChange(w, r, "remove")
}

// whitelistChange runs an add or remove.
func (a *API) whitelistChange(w http.ResponseWriter, r *http.Request, action string) {
	server, _, ok := a.gameDir(w, r, ScopeServersWrite)
	if !ok {
		return
	}

	name := r.PathValue("name")
	if name == "" {
		var req playerNameRequest
		if !decodeBody(w, r, &req) {
			return
		}
		name = req.Name
	}
	if err := gamefiles.ValidatePlayerName(name); err != nil {
		writeFieldError(w, "name", err.Error())
		return
	}

	// The whitelist file is only rewritten by the running server, so an
	// offline change would be lost the next time it starts.
	if !a.requireRunning(w, r, server.ID) {
		return
	}

	if err := a.sendGameCommand(r.Context(), server.ID,
		fmt.Sprintf("whitelist %s %s", action, name)); err != nil {
		a.writeRunnerError(w, server.ID, err)
		return
	}

	a.auditGame(r, "whitelist."+action, name)
	w.WriteHeader(http.StatusNoContent)
}

// handleSetWhitelistState serves PATCH /servers/{id}/whitelist.
func (a *API) handleSetWhitelistState(w http.ResponseWriter, r *http.Request) {
	server, _, ok := a.gameDir(w, r, ScopeServersWrite)
	if !ok {
		return
	}

	var req whitelistStateRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if !a.requireRunning(w, r, server.ID) {
		return
	}

	command := "whitelist off"
	if req.Enabled {
		command = "whitelist on"
	}
	if err := a.sendGameCommand(r.Context(), server.ID, command); err != nil {
		a.writeRunnerError(w, server.ID, err)
		return
	}

	a.auditGame(r, "whitelist.state", command)
	w.WriteHeader(http.StatusNoContent)
}

// --- operators ---

// handleListOps serves GET /servers/{id}/ops.
func (a *API) handleListOps(w http.ResponseWriter, r *http.Request) {
	_, dir, ok := a.gameDir(w, r, ScopeServersRead)
	if !ok {
		return
	}

	players, err := gamefiles.LoadOps(dir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not read the operator list")
		return
	}
	if players == nil {
		players = []gamefiles.Player{}
	}
	writeJSON(w, http.StatusOK, listResponse[gamefiles.Player]{Items: players})
}

// handleAddOp serves POST /servers/{id}/ops.
func (a *API) handleAddOp(w http.ResponseWriter, r *http.Request) {
	a.opChange(w, r, "op")
}

// handleRemoveOp serves DELETE /servers/{id}/ops/{name}.
func (a *API) handleRemoveOp(w http.ResponseWriter, r *http.Request) {
	a.opChange(w, r, "deop")
}

func (a *API) opChange(w http.ResponseWriter, r *http.Request, command string) {
	server, _, ok := a.gameDir(w, r, ScopeServersWrite)
	if !ok {
		return
	}

	name := r.PathValue("name")
	if name == "" {
		var req playerNameRequest
		if !decodeBody(w, r, &req) {
			return
		}
		name = req.Name
	}
	if err := gamefiles.ValidatePlayerName(name); err != nil {
		writeFieldError(w, "name", err.Error())
		return
	}
	if !a.requireRunning(w, r, server.ID) {
		return
	}

	if err := a.sendGameCommand(r.Context(), server.ID, command+" "+name); err != nil {
		a.writeRunnerError(w, server.ID, err)
		return
	}

	a.auditGame(r, "player."+command, name)
	w.WriteHeader(http.StatusNoContent)
}

// --- settings ---

// handleGetSettings serves GET /servers/{id}/settings.
func (a *API) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	_, dir, ok := a.gameDir(w, r, ScopeServersRead)
	if !ok {
		return
	}

	props, err := gamefiles.LoadProperties(dir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError,
			"could not read server.properties")
		return
	}

	managed := make(map[string]string)
	for _, key := range props.Keys() {
		if reason, isManaged := gamefiles.ManagedReason(key); isManaged {
			managed[key] = reason
		}
	}

	writeJSON(w, http.StatusOK, settingsResponse{
		Values:  props.All(),
		Schema:  gamefiles.Schema(),
		Managed: managed,
	})
}

// handlePatchSettings serves PATCH /servers/{id}/settings.
func (a *API) handlePatchSettings(w http.ResponseWriter, r *http.Request) {
	server, dir, ok := a.gameDir(w, r, ScopeServersWrite)
	if !ok {
		return
	}

	var req patchSettingsRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if len(req) == 0 {
		writeFieldError(w, "settings", "nothing to change")
		return
	}

	props, err := gamefiles.LoadProperties(dir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError,
			"could not read server.properties")
		return
	}

	// Everything is validated before anything is written, so a request with
	// one bad value does not leave half of it applied.
	for key, value := range req {
		if reason, isManaged := gamefiles.ManagedReason(key); isManaged {
			writeErrorDetails(w, http.StatusBadRequest, CodeValidationFailed,
				key+": "+reason, map[string]any{"field": key, "reason": "managed"})
			return
		}
		if err := gamefiles.ValidateValue(key, value); err != nil {
			writeFieldError(w, key, err.Error())
			return
		}
	}

	for key, value := range req {
		props.Set(key, value)
	}

	if err := props.Save(dir); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError,
			"could not write server.properties")
		return
	}

	changed := make([]string, 0, len(req))
	for key := range req {
		changed = append(changed, key)
	}
	sort.Strings(changed)
	a.auditGame(r, "settings.update", strings.Join(changed, ","))

	// The server reads server.properties once, at startup.
	restartRequired := false
	if status, err := a.serverStatus(r.Context(), server.ID); err == nil && status.IsActive() {
		restartRequired = true
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"values":           props.All(),
		"restart_required": restartRequired,
	})
}

// auditGame records a player or settings action.
func (a *API) auditGame(r *http.Request, action, target string) {
	if principal, ok := principalFrom(r.Context()); ok {
		a.audit(r, principal.UserID, action, r.PathValue("id"), target)
	}
}
