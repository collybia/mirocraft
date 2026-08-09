package api

import (
	"errors"
	"net/http"

	"github.com/collybia/mirocraft/internal/core"
)

// coreResponse is one core the panel can offer.
type coreResponse struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Runtime string `json:"runtime"`
	// Loader and ContentDir say what add-ons this core takes and where they
	// go. An empty loader means it takes none, which is the honest answer for
	// vanilla: a plugin dropped beside it is simply never read.
	Loader     string `json:"loader,omitempty"`
	ContentDir string `json:"content_dir,omitempty"`
}

type coreVersionResponse struct {
	ID        string `json:"id"`
	Channel   string `json:"channel"`
	JavaMajor int    `json:"java_major"`
}

// handleListCores serves GET /cores.
//
// The panel builds its core picker from this rather than from a list of its
// own. A hardcoded list is a list that drifts: it offered forge and neoforge
// for a while before either existed, and picking one failed at provisioning
// time with an error about an unknown core.
func (a *API) handleListCores(w http.ResponseWriter, r *http.Request) {
	if a.cores == nil {
		writeJSON(w, http.StatusOK, listResponse[coreResponse]{Items: []coreResponse{}})
		return
	}

	providers := a.cores.List()
	items := make([]coreResponse, 0, len(providers))
	for _, provider := range providers {
		content := provider.Content()
		items = append(items, coreResponse{
			ID:         provider.ID(),
			Name:       provider.Name(),
			Kind:       string(provider.Kind()),
			Runtime:    string(provider.Runtime()),
			Loader:     content.Loader,
			ContentDir: content.Dir,
		})
	}
	writeJSON(w, http.StatusOK, listResponse[coreResponse]{Items: items})
}

// handleListCoreVersions serves GET /cores/{core}/versions.
//
// Answered from upstream, so it is as slow as upstream is. The providers cache
// what they can; this endpoint does not add a second layer, because a stale
// version list is exactly the thing that makes a newly released Minecraft
// version look unavailable.
func (a *API) handleListCoreVersions(w http.ResponseWriter, r *http.Request) {
	if a.cores == nil {
		writeError(w, http.StatusServiceUnavailable, CodeInternalError, "no cores are configured on this node")
		return
	}

	provider, err := a.cores.Get(r.PathValue("core"))
	if err != nil {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "no such core")
		return
	}

	versions, err := provider.Versions(r.Context())
	if err != nil {
		a.log.Warn("listing core versions failed",
			"core", provider.ID(), "error", err)
		// The upstream API being down is not this panel being broken, and the
		// message says which: an operator who reads "could not reach" looks at
		// their network rather than at the panel's logs.
		writeError(w, http.StatusBadGateway, CodeInternalError,
			"could not reach the "+provider.Name()+" API")
		return
	}

	items := make([]coreVersionResponse, 0, len(versions))
	for _, version := range versions {
		items = append(items, coreVersionResponse{
			ID:        version.ID,
			Channel:   version.Channel,
			JavaMajor: version.JavaMajor,
		})
	}
	writeJSON(w, http.StatusOK, listResponse[coreVersionResponse]{Items: items})
}

// validateCore rejects a core the daemon cannot serve, before a server record
// is written for it.
//
// Without this a server can be created naming a core that does not exist and
// fails only when someone tries to start it — with an error about
// provisioning, at the moment they least expect it.
func (a *API) validateCore(id string) error {
	if a.cores == nil {
		return nil
	}
	if _, err := a.cores.Get(id); err != nil {
		if errors.Is(err, core.ErrUnknownProvider) {
			return err
		}
		return err
	}
	return nil
}
