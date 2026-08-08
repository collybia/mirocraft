package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/temertika/mirocraft/internal/store"
)

// Token scopes from docs/API.md.
const (
	ScopeServersRead    = "servers:read"
	ScopeServersWrite   = "servers:write"
	ScopeServersPower   = "servers:power"
	ScopeServersConsole = "servers:console"
	ScopeFilesRead      = "files:read"
	ScopeFilesWrite     = "files:write"
	ScopeBackupsRead    = "backups:read"
	ScopeBackupsWrite   = "backups:write"
	ScopeAdminAll       = "admin:*"
)

// AllScopes is every scope a token may hold, used to validate requests and to
// grant session tokens the full set for their role.
var AllScopes = []string{
	ScopeServersRead, ScopeServersWrite, ScopeServersPower, ScopeServersConsole,
	ScopeFilesRead, ScopeFilesWrite, ScopeBackupsRead, ScopeBackupsWrite,
	ScopeAdminAll,
}

// Roles.
const (
	RoleAdmin = store.RoleAdmin
	RoleUser  = store.RoleUser
)

// Principal is the authenticated caller behind a request.
type Principal struct {
	UserID  string
	Email   string
	Role    string
	Scopes  []string
	TokenID string
}

// HasScope reports whether the principal may perform an action. admin:* grants
// every scope, matching the documented behaviour of admin tokens.
func (p *Principal) HasScope(scope string) bool {
	if p == nil {
		return false
	}
	for _, s := range p.Scopes {
		if s == scope || s == ScopeAdminAll {
			return true
		}
	}
	return false
}

// IsAdmin reports whether the principal holds the admin role.
func (p *Principal) IsAdmin() bool { return p != nil && p.Role == RoleAdmin }

type contextKey int

const principalKey contextKey = iota

func principalFrom(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalKey).(*Principal)
	return p, ok
}

func withPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

// authenticate is middleware that requires a valid Bearer token.
func (a *API) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := bearerToken(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, CodeUnauthorized, "missing bearer token")
			return
		}

		principal, err := a.principalForToken(r.Context(), raw)
		if err != nil {
			writeError(w, http.StatusUnauthorized, CodeUnauthorized, "invalid or expired token")
			return
		}

		next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), principal)))
	})
}

// principalForToken resolves a raw token into a principal, rejecting expired
// tokens and blocked users.
func (a *API) principalForToken(ctx context.Context, raw string) (*Principal, error) {
	token, err := a.store.Tokens.GetByHash(ctx, store.HashToken(raw))
	if err != nil {
		return nil, err
	}
	if token.Expired(time.Now()) {
		return nil, errors.New("token expired")
	}

	user, err := a.store.Users.GetByID(ctx, token.UserID)
	if err != nil {
		return nil, err
	}
	if user.Blocked {
		return nil, errors.New("user is blocked")
	}

	// Best-effort bookkeeping: a failure here must not fail the request.
	if err := a.store.Tokens.TouchLastUsed(ctx, token.ID, time.Now()); err != nil {
		a.log.Debug("touching token failed", "error", err)
	}

	return &Principal{
		UserID:  user.ID,
		Email:   user.Email,
		Role:    user.Role,
		Scopes:  token.Scopes,
		TokenID: token.ID,
	}, nil
}

func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	token := strings.TrimSpace(header[len(prefix):])
	return token, token != ""
}

// requireAdmin is middleware that rejects non-admin principals.
func (a *API) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := principalFrom(r.Context())
		if !ok || !principal.IsAdmin() {
			writeError(w, http.StatusForbidden, CodeForbidden, "admin role required")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authorizeServer checks that the principal holds the scope and may access the
// server. It writes the error response itself and reports whether the request
// may continue.
//
// A principal without access gets 404 rather than 403 for a server it does not
// own, so the API does not leak which server ids exist.
func (a *API) authorizeServer(w http.ResponseWriter, r *http.Request, serverID, scope string) (*Principal, bool) {
	principal, ok := principalFrom(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "not authenticated")
		return nil, false
	}
	if !principal.HasScope(scope) {
		writeError(w, http.StatusForbidden, CodeForbidden, "token is missing the "+scope+" scope")
		return nil, false
	}

	server, err := a.store.Servers.GetByID(r.Context(), serverID)
	if err != nil {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "server "+serverID+" does not exist")
		return nil, false
	}
	if !principal.IsAdmin() && server.OwnerID != principal.UserID {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "server "+serverID+" does not exist")
		return nil, false
	}

	return principal, true
}

// validateScopes rejects scopes outside the documented set, so a typo cannot
// mint a token that silently grants nothing.
func validateScopes(scopes []string) error {
	if len(scopes) == 0 {
		return errors.New("at least one scope is required")
	}
	for _, s := range scopes {
		known := false
		for _, valid := range AllScopes {
			if s == valid {
				known = true
				break
			}
		}
		if !known {
			return errors.New("unknown scope " + s)
		}
	}
	return nil
}

// scopesForRole returns the scopes a session token gets. Admins get admin:*,
// which subsumes everything; regular users get everything but that.
func scopesForRole(role string) []string {
	if role == RoleAdmin {
		return append([]string{}, AllScopes...)
	}
	scopes := make([]string, 0, len(AllScopes)-1)
	for _, s := range AllScopes {
		if s == ScopeAdminAll {
			continue
		}
		scopes = append(scopes, s)
	}
	return scopes
}
