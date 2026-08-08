package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"github.com/collybia/mirocraft/internal/store"
)

// MinPasswordLength is the shortest password accepted. Length is the only
// rule: composition requirements push people towards predictable patterns
// without adding real strength.
const MinPasswordLength = 10

// --- wire types ---

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	TOTP     string `json:"totp,omitempty"`
}

type loginResponse struct {
	Token     string       `json:"token"`
	ExpiresAt time.Time    `json:"expires_at"`
	User      userResponse `json:"user"`
}

type userResponse struct {
	ID         string    `json:"id"`
	Email      string    `json:"email"`
	Role       string    `json:"role"`
	Theme      string    `json:"theme"`
	Blocked    bool      `json:"blocked"`
	MaxServers int       `json:"max_servers"`
	MaxRAMMb   int       `json:"max_ram_mb"`
	MaxDiskMb  int       `json:"max_disk_mb"`
	CreatedAt  time.Time `json:"created_at"`
}

type meResponse struct {
	userResponse
	Scopes []string `json:"scopes"`
}

type tokenResponse struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	Kind       string     `json:"kind"`
	ExpiresAt  *time.Time `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

// createTokenResponse carries the token value, which is shown exactly once.
type createTokenResponse struct {
	tokenResponse
	Token string `json:"token"`
}

type createTokenRequest struct {
	Name      string     `json:"name"`
	Scopes    []string   `json:"scopes"`
	ExpiresAt *time.Time `json:"expires_at"`
}

type listResponse[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

type patchMeRequest struct {
	Email       *string `json:"email"`
	Password    *string `json:"password"`
	OldPassword *string `json:"old_password"`
	Theme       *string `json:"theme"`
}

type createUserRequest struct {
	Email      string `json:"email"`
	Password   string `json:"password"`
	Role       string `json:"role"`
	MaxServers int    `json:"max_servers"`
	MaxRAMMb   int    `json:"max_ram_mb"`
	MaxDiskMb  int    `json:"max_disk_mb"`
}

type patchUserRequest struct {
	Role       *string `json:"role"`
	Blocked    *bool   `json:"blocked"`
	Password   *string `json:"password"`
	MaxServers *int    `json:"max_servers"`
	MaxRAMMb   *int    `json:"max_ram_mb"`
	MaxDiskMb  *int    `json:"max_disk_mb"`
}

func toUserResponse(u *store.User) userResponse {
	return userResponse{
		ID: u.ID, Email: u.Email, Role: u.Role, Theme: u.Theme, Blocked: u.Blocked,
		MaxServers: u.MaxServers, MaxRAMMb: u.MaxRAMMb, MaxDiskMb: u.MaxDiskMb,
		CreatedAt: u.CreatedAt,
	}
}

func toTokenResponse(t *store.Token) tokenResponse {
	return tokenResponse{
		ID: t.ID, Name: t.Name, Scopes: t.Scopes, Kind: t.Kind,
		ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt, CreatedAt: t.CreatedAt,
	}
}

// --- auth ---

// handleLogin serves POST /auth/login.
func (a *API) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decodeBody(w, r, &req) {
		return
	}

	user, err := a.store.Users.GetByEmail(r.Context(), req.Email)
	if err != nil {
		// Wrong email and wrong password give the same answer, so the endpoint
		// cannot be used to enumerate registered addresses.
		a.failLogin(w, r, req.Email, "unknown email")
		return
	}
	if err := store.CheckPassword(user.PasswordHash, req.Password); err != nil {
		a.failLogin(w, r, req.Email, "bad password")
		return
	}
	if user.Blocked {
		a.failLogin(w, r, req.Email, "user blocked")
		return
	}

	value, expiresAt, err := a.issueSession(r.Context(), user)
	if err != nil {
		a.log.Error("issuing session failed", slog.String("error", err.Error()))
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not create a session")
		return
	}

	a.audit(r, user.ID, "auth.login", user.ID, "")
	writeJSON(w, http.StatusOK, loginResponse{
		Token: value, ExpiresAt: expiresAt, User: toUserResponse(user),
	})
}

// failLogin returns the single indistinguishable failure response.
func (a *API) failLogin(w http.ResponseWriter, r *http.Request, email, reason string) {
	a.log.Warn("login failed",
		slog.String("email", email),
		slog.String("reason", reason),
		slog.String("ip", clientIP(r)))
	writeError(w, http.StatusUnauthorized, CodeUnauthorized, "invalid email or password")
}

func (a *API) issueSession(ctx context.Context, user *store.User) (string, time.Time, error) {
	value, hash, err := store.GenerateToken()
	if err != nil {
		return "", time.Time{}, err
	}

	expiresAt := time.Now().Add(a.sessionTTL)
	token := &store.Token{
		UserID: user.ID, Name: "session", Hash: hash,
		Scopes: scopesForRole(user.Role), Kind: store.TokenKindSession,
		ExpiresAt: &expiresAt,
	}
	if err := a.store.Tokens.Create(ctx, token); err != nil {
		return "", time.Time{}, err
	}
	return value, expiresAt, nil
}

// handleLogout serves POST /auth/logout, revoking the calling token.
func (a *API) handleLogout(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFrom(r.Context())

	if err := a.store.Tokens.Delete(r.Context(), principal.TokenID); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not revoke the token")
		return
	}

	a.audit(r, principal.UserID, "auth.logout", principal.TokenID, "")
	w.WriteHeader(http.StatusNoContent)
}

// handleRefresh serves POST /auth/refresh: issues a new session token and
// revokes the current one, so a stolen token has a bounded life.
func (a *API) handleRefresh(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFrom(r.Context())

	user, err := a.store.Users.GetByID(r.Context(), principal.UserID)
	if err != nil {
		writeError(w, http.StatusUnauthorized, CodeUnauthorized, "user no longer exists")
		return
	}

	value, expiresAt, err := a.issueSession(r.Context(), user)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not refresh the session")
		return
	}
	if err := a.store.Tokens.Delete(r.Context(), principal.TokenID); err != nil {
		a.log.Warn("revoking the old session failed", slog.String("error", err.Error()))
	}

	writeJSON(w, http.StatusOK, loginResponse{
		Token: value, ExpiresAt: expiresAt, User: toUserResponse(user),
	})
}

// handleMe serves GET /auth/me and GET /users/me.
func (a *API) handleMe(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFrom(r.Context())

	user, err := a.store.Users.GetByID(r.Context(), principal.UserID)
	if err != nil {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "user not found")
		return
	}

	writeJSON(w, http.StatusOK, meResponse{
		userResponse: toUserResponse(user),
		Scopes:       principal.Scopes,
	})
}

// handlePatchMe serves PATCH /users/me.
func (a *API) handlePatchMe(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFrom(r.Context())

	var req patchMeRequest
	if !decodeBody(w, r, &req) {
		return
	}

	user, err := a.store.Users.GetByID(r.Context(), principal.UserID)
	if err != nil {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "user not found")
		return
	}

	if req.Email != nil {
		if err := validateEmail(*req.Email); err != nil {
			writeFieldError(w, "email", err.Error())
			return
		}
		user.Email = *req.Email
	}

	if req.Password != nil {
		// Changing a password requires proving the current one, so a stolen
		// session cannot lock the owner out.
		if req.OldPassword == nil {
			writeFieldError(w, "old_password", "the current password is required to set a new one")
			return
		}
		if err := store.CheckPassword(user.PasswordHash, *req.OldPassword); err != nil {
			writeFieldError(w, "old_password", "current password is wrong")
			return
		}
		if err := validatePassword(*req.Password); err != nil {
			writeFieldError(w, "password", err.Error())
			return
		}
		hash, err := store.HashPassword(*req.Password)
		if err != nil {
			writeError(w, http.StatusInternalServerError, CodeInternalError, "could not hash the password")
			return
		}
		user.PasswordHash = hash
	}

	if req.Theme != nil {
		if err := validateThemeChoice(*req.Theme); err != nil {
			writeFieldError(w, "theme", err.Error())
			return
		}
		user.Theme = *req.Theme
	}

	if err := a.store.Users.Update(r.Context(), user); err != nil {
		if errors.Is(err, store.ErrEmailTaken) {
			writeFieldError(w, "email", "this email is already registered")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not update the profile")
		return
	}

	a.audit(r, user.ID, "user.update_self", user.ID, "")
	writeJSON(w, http.StatusOK, toUserResponse(user))
}

// --- api tokens ---

// handleListTokens serves GET /auth/tokens.
func (a *API) handleListTokens(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFrom(r.Context())

	tokens, err := a.store.Tokens.ListByUser(r.Context(), principal.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not list tokens")
		return
	}

	items := make([]tokenResponse, 0, len(tokens))
	for _, t := range tokens {
		items = append(items, toTokenResponse(t))
	}
	writeJSON(w, http.StatusOK, listResponse[tokenResponse]{Items: items})
}

// handleCreateToken serves POST /auth/tokens. The value is returned once and
// never again, because only its hash is stored.
func (a *API) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFrom(r.Context())

	var req createTokenRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeFieldError(w, "name", "a name is required")
		return
	}
	if err := validateScopes(req.Scopes); err != nil {
		writeFieldError(w, "scopes", err.Error())
		return
	}
	// A token must not be able to grant more than its creator holds.
	for _, scope := range req.Scopes {
		if !principal.HasScope(scope) {
			writeFieldError(w, "scopes", "you cannot grant the scope "+scope)
			return
		}
	}
	if req.ExpiresAt != nil && !req.ExpiresAt.After(time.Now()) {
		writeFieldError(w, "expires_at", "expiry must be in the future")
		return
	}

	value, hash, err := store.GenerateToken()
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not generate a token")
		return
	}

	token := &store.Token{
		UserID: principal.UserID, Name: req.Name, Hash: hash,
		Scopes: req.Scopes, Kind: store.TokenKindAPI, ExpiresAt: req.ExpiresAt,
	}
	if err := a.store.Tokens.Create(r.Context(), token); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not create the token")
		return
	}

	a.audit(r, principal.UserID, "token.create", token.ID, req.Name)
	writeJSON(w, http.StatusCreated, createTokenResponse{
		tokenResponse: toTokenResponse(token),
		Token:         value,
	})
}

// handleDeleteToken serves DELETE /auth/tokens/{id}.
func (a *API) handleDeleteToken(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFrom(r.Context())
	id := r.PathValue("id")

	token, err := a.store.Tokens.GetByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "token not found")
		return
	}
	// Another user's token is reported as missing rather than forbidden, so
	// token ids cannot be probed.
	if token.UserID != principal.UserID && !principal.IsAdmin() {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "token not found")
		return
	}

	if err := a.store.Tokens.Delete(r.Context(), id); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not revoke the token")
		return
	}

	a.audit(r, principal.UserID, "token.revoke", id, "")
	w.WriteHeader(http.StatusNoContent)
}

// --- admin users ---

// handleListUsers serves GET /admin/users.
func (a *API) handleListUsers(w http.ResponseWriter, r *http.Request) {
	users, err := a.store.Users.List(r.Context(), 200)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not list users")
		return
	}

	items := make([]userResponse, 0, len(users))
	for _, u := range users {
		items = append(items, toUserResponse(u))
	}
	writeJSON(w, http.StatusOK, listResponse[userResponse]{Items: items})
}

// handleCreateUser serves POST /admin/users.
func (a *API) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFrom(r.Context())

	var req createUserRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if err := validateEmail(req.Email); err != nil {
		writeFieldError(w, "email", err.Error())
		return
	}
	if err := validatePassword(req.Password); err != nil {
		writeFieldError(w, "password", err.Error())
		return
	}
	role := req.Role
	if role == "" {
		role = RoleUser
	}
	if role != RoleAdmin && role != RoleUser {
		writeFieldError(w, "role", "role must be admin or user")
		return
	}

	hash, err := store.HashPassword(req.Password)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not hash the password")
		return
	}

	user := &store.User{
		Email: req.Email, PasswordHash: hash, Role: role,
		MaxServers: req.MaxServers, MaxRAMMb: req.MaxRAMMb, MaxDiskMb: req.MaxDiskMb,
	}
	if err := a.store.Users.Create(r.Context(), user); err != nil {
		if errors.Is(err, store.ErrEmailTaken) {
			writeFieldError(w, "email", "this email is already registered")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not create the user")
		return
	}

	a.audit(r, principal.UserID, "user.create", user.ID, user.Email)
	writeJSON(w, http.StatusCreated, toUserResponse(user))
}

// handlePatchUser serves PATCH /admin/users/{id}.
func (a *API) handlePatchUser(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFrom(r.Context())
	id := r.PathValue("id")

	var req patchUserRequest
	if !decodeBody(w, r, &req) {
		return
	}

	user, err := a.store.Users.GetByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "user not found")
		return
	}

	// An admin must not be able to strip or block their own account and lock
	// the panel out of its last administrator by accident.
	if user.ID == principal.UserID {
		if req.Role != nil && *req.Role != RoleAdmin {
			writeFieldError(w, "role", "you cannot remove your own admin role")
			return
		}
		if req.Blocked != nil && *req.Blocked {
			writeFieldError(w, "blocked", "you cannot block yourself")
			return
		}
	}

	if req.Role != nil {
		if *req.Role != RoleAdmin && *req.Role != RoleUser {
			writeFieldError(w, "role", "role must be admin or user")
			return
		}
		user.Role = *req.Role
	}
	if req.Blocked != nil {
		user.Blocked = *req.Blocked
	}
	if req.MaxServers != nil {
		user.MaxServers = *req.MaxServers
	}
	if req.MaxRAMMb != nil {
		user.MaxRAMMb = *req.MaxRAMMb
	}
	if req.MaxDiskMb != nil {
		user.MaxDiskMb = *req.MaxDiskMb
	}
	if req.Password != nil {
		if err := validatePassword(*req.Password); err != nil {
			writeFieldError(w, "password", err.Error())
			return
		}
		hash, err := store.HashPassword(*req.Password)
		if err != nil {
			writeError(w, http.StatusInternalServerError, CodeInternalError, "could not hash the password")
			return
		}
		user.PasswordHash = hash
	}

	if err := a.store.Users.Update(r.Context(), user); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not update the user")
		return
	}

	a.audit(r, principal.UserID, "user.update", user.ID, "")
	writeJSON(w, http.StatusOK, toUserResponse(user))
}

// handleDeleteUser serves DELETE /admin/users/{id}.
func (a *API) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFrom(r.Context())
	id := r.PathValue("id")

	if id == principal.UserID {
		writeFieldError(w, "id", "you cannot delete your own account")
		return
	}

	if err := a.store.Users.Delete(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, CodeServerNotFound, "user not found")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not delete the user")
		return
	}

	a.audit(r, principal.UserID, "user.delete", id, "")
	w.WriteHeader(http.StatusNoContent)
}

// --- helpers ---

// decodeBody reads a JSON request body, writing the error response itself.
// An empty body decodes to the zero value, which PATCH endpoints treat as
// "change nothing".
func decodeBody(w http.ResponseWriter, r *http.Request, target any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(target); err != nil {
		if errors.Is(err, io.EOF) {
			return true
		}
		writeError(w, http.StatusBadRequest, CodeValidationFailed, "body must be a json object")
		return false
	}
	return true
}

func writeFieldError(w http.ResponseWriter, field, reason string) {
	writeErrorDetails(w, http.StatusBadRequest, CodeValidationFailed, reason,
		map[string]any{"field": field, "reason": reason})
}

func validateEmail(email string) error {
	email = strings.TrimSpace(email)
	if email == "" {
		return errors.New("email is required")
	}
	if _, err := mail.ParseAddress(email); err != nil {
		return errors.New("email is not a valid address")
	}
	return nil
}

func validatePassword(password string) error {
	if len([]rune(password)) < MinPasswordLength {
		return errors.New("password must be at least 10 characters")
	}
	return nil
}

// audit records a mutating action, best effort: failing to write the log must
// not fail the request that succeeded.
func (a *API) audit(r *http.Request, userID, action, target, details string) {
	err := a.store.Audit.Append(r.Context(), &store.AuditEntry{
		UserID: userID, Action: action, Target: target,
		IP: clientIP(r), Details: details,
	})
	if err != nil {
		a.log.Warn("writing the audit log failed",
			slog.String("action", action), slog.String("error", err.Error()))
	}
}
