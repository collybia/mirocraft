package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/collybia/mirocraft/internal/store"
)

// Error codes the linking flow adds.
const (
	CodeLinkInvalid = "link_code_invalid"
	CodeLinkTaken   = "link_taken"
)

type integrationResponse struct {
	Provider   string    `json:"provider"`
	ExternalID string    `json:"external_id"`
	CreatedAt  time.Time `json:"created_at"`
}

type linkCodeResponse struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expires_at"`
}

type redeemLinkRequest struct {
	Code       string `json:"code"`
	ExternalID string `json:"external_id"`
}

type redeemLinkResponse struct {
	Provider   string `json:"provider"`
	ExternalID string `json:"external_id"`
	// The account the code belonged to, so a bot can greet the right person.
	UserID string `json:"user_id"`
	Email  string `json:"email"`
}

// handleListIntegrations serves GET /integrations.
func (a *API) handleListIntegrations(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "not authenticated")
		return
	}

	links, err := a.store.Integrations.ListForUser(r.Context(), principal.UserID)
	if err != nil {
		a.log.Error("listing integrations failed", "error", err)
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not list linked accounts")
		return
	}

	items := make([]integrationResponse, 0, len(links))
	for _, link := range links {
		items = append(items, integrationResponse{
			Provider: link.Provider, ExternalID: link.ExternalID, CreatedAt: link.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, listResponse[integrationResponse]{Items: items})
}

// handleCreateLinkCode serves POST /integrations/{provider}/code.
//
// The code is returned once and never stored in the clear, like every other
// credential here. It is issued to the calling account: whoever redeems it in
// a chat becomes linked to this account, which is why it is short-lived.
func (a *API) handleCreateLinkCode(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "not authenticated")
		return
	}
	// A bot must not be able to mint a code for the person it is acting for:
	// that would let it link a second chat account to them.
	if principal.Delegate != nil {
		writeError(w, http.StatusForbidden, CodeForbidden,
			"a linking code has to be requested from the panel itself")
		return
	}

	provider, ok := validProvider(w, r)
	if !ok {
		return
	}

	code, expiresAt, err := a.store.Integrations.IssueCode(r.Context(), provider, principal.UserID)
	if err != nil {
		a.log.Error("issuing a linking code failed", "error", err)
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not issue a code")
		return
	}

	a.audit(r, principal.UserID, "integration.code."+provider, "", "")
	writeJSON(w, http.StatusCreated, linkCodeResponse{Code: code, ExpiresAt: expiresAt})
}

// handleRedeemLink serves POST /integrations/{provider}/link.
//
// Called by a bot with its own token: it carries the code a person typed in
// the chat and the id of the chat account that typed it.
func (a *API) handleRedeemLink(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "not authenticated")
		return
	}
	// The same grant that lets a token act for a linked account lets it
	// create the link. Checked by name rather than through HasScope, so
	// admin:* does not sweep it up.
	if !hasExactScope(principal, ScopeIntegrationsAct) {
		writeError(w, http.StatusForbidden, CodeForbidden,
			"token is missing the "+ScopeIntegrationsAct+" scope")
		return
	}
	if principal.Delegate != nil {
		writeError(w, http.StatusForbidden, CodeForbidden,
			"a link cannot be created on behalf of a linked account")
		return
	}

	provider, ok := validProvider(w, r)
	if !ok {
		return
	}

	var req redeemLinkRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if store.NormalizeCode(req.Code) == "" {
		writeFieldError(w, "code", "code is required")
		return
	}
	if req.ExternalID == "" {
		writeFieldError(w, "external_id", "external_id is required")
		return
	}

	link, err := a.store.Integrations.Redeem(r.Context(), provider, req.Code, req.ExternalID)
	switch {
	case errors.Is(err, store.ErrCodeInvalid):
		writeError(w, http.StatusBadRequest, CodeLinkInvalid, "the code is invalid or has expired")
		return
	case errors.Is(err, store.ErrLinkTaken):
		writeError(w, http.StatusConflict, CodeLinkTaken, "that account is already linked to another user")
		return
	case err != nil:
		a.log.Error("redeeming a linking code failed", "error", err)
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not link the account")
		return
	}

	user, err := a.store.Users.GetByID(r.Context(), link.UserID)
	if err != nil {
		a.log.Error("reading the linked user failed", "error", err)
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not link the account")
		return
	}

	a.audit(r, link.UserID, "integration.link."+provider, "", link.ExternalID)
	writeJSON(w, http.StatusCreated, redeemLinkResponse{
		Provider: link.Provider, ExternalID: link.ExternalID,
		UserID: user.ID, Email: user.Email,
	})
}

// handleUnlink serves DELETE /integrations/{provider}.
//
// Allowed for a delegated caller too: someone should be able to say "forget
// me" from the chat they are in, without opening the panel.
func (a *API) handleUnlink(w http.ResponseWriter, r *http.Request) {
	principal, ok := principalFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "not authenticated")
		return
	}

	provider, ok := validProvider(w, r)
	if !ok {
		return
	}

	err := a.store.Integrations.Unlink(r.Context(), provider, principal.UserID)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "no "+provider+" account is linked")
		return
	}
	if err != nil {
		a.log.Error("unlinking failed", "error", err)
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not unlink the account")
		return
	}

	a.audit(r, principal.UserID, "integration.unlink."+provider, "", "")
	w.WriteHeader(http.StatusNoContent)
}

// validProvider reads and checks the {provider} path value.
func validProvider(w http.ResponseWriter, r *http.Request) (string, bool) {
	provider := r.PathValue("provider")
	switch provider {
	case store.ProviderDiscord, store.ProviderTelegram:
		return provider, true
	default:
		writeFieldError(w, "provider", "provider must be discord or telegram")
		return "", false
	}
}
