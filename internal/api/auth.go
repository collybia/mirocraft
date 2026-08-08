package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
)

// Token scopes from docs/API.md.
const (
	ScopeServersRead    = "servers:read"
	ScopeServersWrite   = "servers:write"
	ScopeServersPower   = "servers:power"
	ScopeServersConsole = "servers:console"
	ScopeAdminAll       = "admin:*"
)

// Roles.
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
)

// Principal is the authenticated caller behind a request.
type Principal struct {
	UserID string
	Role   string
	Scopes []string
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

// ServerRecord is the ownership information the API needs in order to decide
// whether a principal may touch a server.
type ServerRecord struct {
	ID      string
	OwnerID string
}

// Authenticator resolves a raw bearer token into a principal.
// Task 1.1 replaces the in-memory implementation with the SQLite store; the
// interface is what the API depends on.
type Authenticator interface {
	Authenticate(ctx context.Context, rawToken string) (*Principal, error)
}

// ServerLookup resolves a server id into its ownership record.
type ServerLookup interface {
	LookupServer(ctx context.Context, id string) (*ServerRecord, error)
}

// Authentication and authorization errors.
var (
	ErrInvalidToken = errors.New("invalid or expired token")
	ErrNoSuchServer = errors.New("server not found")
)

type contextKey int

const principalKey contextKey = iota

// principalFrom returns the authenticated principal carried by ctx.
func principalFrom(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalKey).(*Principal)
	return p, ok
}

// withPrincipal is exported to tests through the middleware; kept unexported
// so handlers cannot forge a principal.
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

		principal, err := a.auth.Authenticate(r.Context(), raw)
		if err != nil || principal == nil {
			writeError(w, http.StatusUnauthorized, CodeUnauthorized, "invalid or expired token")
			return
		}

		next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), principal)))
	})
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

	record, err := a.servers.LookupServer(r.Context(), serverID)
	if err != nil || record == nil {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "server "+serverID+" does not exist")
		return nil, false
	}
	if principal.Role != RoleAdmin && record.OwnerID != principal.UserID {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "server "+serverID+" does not exist")
		return nil, false
	}

	return principal, true
}

// HashToken returns the storage form of an API token. Only the hash is ever
// persisted, per the project's security rules.
func HashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// MemoryAuth is an in-memory Authenticator and ServerLookup.
//
// It exists so the console can be built and tested before the SQLite store
// lands in task 1.1; the daemon swaps in the real store without the API
// changing.
type MemoryAuth struct {
	mu      sync.RWMutex
	tokens  map[string]*Principal // key: token hash
	servers map[string]*ServerRecord
}

// NewMemoryAuth returns an empty in-memory store.
func NewMemoryAuth() *MemoryAuth {
	return &MemoryAuth{
		tokens:  make(map[string]*Principal),
		servers: make(map[string]*ServerRecord),
	}
}

// AddToken registers a raw token for a principal. Only its hash is retained.
func (m *MemoryAuth) AddToken(raw string, p *Principal) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens[HashToken(raw)] = p
}

// AddServer registers server ownership.
func (m *MemoryAuth) AddServer(rec *ServerRecord) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.servers[rec.ID] = rec
}

// Authenticate implements Authenticator.
func (m *MemoryAuth) Authenticate(_ context.Context, rawToken string) (*Principal, error) {
	hash := HashToken(rawToken)

	m.mu.RLock()
	defer m.mu.RUnlock()

	// Compared in constant time so a token cannot be recovered by timing the
	// lookup, even though the map lookup itself is not constant time.
	for stored, principal := range m.tokens {
		if subtle.ConstantTimeCompare([]byte(stored), []byte(hash)) == 1 {
			return principal, nil
		}
	}
	return nil, ErrInvalidToken
}

// LookupServer implements ServerLookup.
func (m *MemoryAuth) LookupServer(_ context.Context, id string) (*ServerRecord, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	rec, ok := m.servers[id]
	if !ok {
		return nil, ErrNoSuchServer
	}
	return rec, nil
}
