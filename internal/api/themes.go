package api

import (
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/collybia/mirocraft/internal/store"
)

// ThemeSchema is the version tag on exported and imported themes.
const ThemeSchema = "mirocraft.theme/v1"

// Theme bases. Every theme, built-in or custom, derives from one of these, and
// the base decides what the light/dark toggle in the header does.
const (
	ThemeBaseDark  = "dark"
	ThemeBaseLight = "light"
)

// ThemeSystem follows the operating system's prefers-color-scheme. It is the
// default for new users.
const ThemeSystem = "system"

// customThemePrefix marks a user's own theme in the profile's theme field.
const customThemePrefix = "custom:"

// BuiltinTheme describes a theme shipped with the panel.
type BuiltinTheme struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Base    string `json:"base"`
	Preview struct {
		BG     string `json:"bg"`
		Text   string `json:"text"`
		Accent string `json:"accent"`
	} `json:"preview"`
}

// builtinThemes is the registry the web panel reads. Preview colours are the
// swatches shown in the theme picker; the real palettes live in
// web/src/themes/<id>.css.
var builtinThemes = []BuiltinTheme{
	newBuiltin("dark", "Dark", ThemeBaseDark, "#16181d", "#e6e8ee", "#4ade80"),
	newBuiltin("light", "Light", ThemeBaseLight, "#ffffff", "#1a1d23", "#16a34a"),
	newBuiltin("midnight", "Midnight", ThemeBaseDark, "#000000", "#dbe3f4", "#5b8cff"),
	newBuiltin("grass", "Grass", ThemeBaseDark, "#161b14", "#e4ead9", "#7cb342"),
	newBuiltin("nether", "Nether", ThemeBaseDark, "#1c1210", "#f0e0d8", "#ff6a3d"),
}

func newBuiltin(id, name, base, bg, text, accent string) BuiltinTheme {
	t := BuiltinTheme{ID: id, Name: name, Base: base}
	t.Preview.BG = bg
	t.Preview.Text = text
	t.Preview.Accent = accent
	return t
}

// themeTokens is the whitelist of CSS variables a custom theme may override.
//
// The whitelist is the security boundary for imports: without it, a shared
// theme JSON could inject arbitrary CSS custom properties into the page.
var themeTokens = map[string]struct{}{
	"--bg": {}, "--bg-elevated": {}, "--bg-inset": {},
	"--text": {}, "--text-muted": {}, "--text-faint": {},
	"--accent": {}, "--accent-hover": {}, "--accent-fg": {},
	"--success": {}, "--success-bg": {},
	"--warning": {}, "--warning-bg": {},
	"--danger": {}, "--danger-bg": {},
	"--info": {}, "--info-bg": {},
	"--border": {}, "--border-strong": {},
	"--radius": {}, "--radius-sm": {}, "--radius-lg": {},
	"--console-bg": {}, "--console-text": {}, "--console-error": {},
	"--console-warn": {}, "--console-info": {}, "--console-debug": {},
	"--console-timestamp": {},
}

// Values are restricted to plain colours and lengths. Anything that could
// carry a URL, a function call or a semicolon is rejected outright rather than
// sanitised, because sanitising CSS correctly is a losing game.
var (
	colorPattern  = regexp.MustCompile(`^#[0-9a-fA-F]{3,8}$`)
	rgbPattern    = regexp.MustCompile(`^rgba?\(\s*\d{1,3}\s*,\s*\d{1,3}\s*,\s*\d{1,3}\s*(,\s*(0|1|0?\.\d+)\s*)?\)$`)
	hslPattern    = regexp.MustCompile(`^hsla?\(\s*\d{1,3}\s*,\s*\d{1,3}%\s*,\s*\d{1,3}%\s*(,\s*(0|1|0?\.\d+)\s*)?\)$`)
	lengthPattern = regexp.MustCompile(`^\d{1,3}(\.\d+)?(px|rem|em|%)$`)
)

// ThemeDocument is the export and import format for a custom theme.
type ThemeDocument struct {
	Schema string            `json:"schema"`
	Name   string            `json:"name"`
	Base   string            `json:"base"`
	Vars   map[string]string `json:"vars"`
}

type customThemeResponse struct {
	ID string `json:"id"`
	ThemeDocument
}

func toThemeDocument(t *store.CustomTheme) ThemeDocument {
	return ThemeDocument{
		Schema: ThemeSchema, Name: t.Name, Base: t.Base, Vars: t.Vars,
	}
}

func toCustomThemeResponse(t *store.CustomTheme) customThemeResponse {
	return customThemeResponse{ID: t.ID, ThemeDocument: toThemeDocument(t)}
}

// validateThemeChoice checks a value for the profile's theme field.
func validateThemeChoice(choice string) error {
	if choice == ThemeSystem {
		return nil
	}
	if strings.HasPrefix(choice, customThemePrefix) {
		if strings.TrimPrefix(choice, customThemePrefix) == "" {
			return errors.New("custom theme id is missing")
		}
		return nil
	}
	for _, t := range builtinThemes {
		if t.ID == choice {
			return nil
		}
	}
	return errors.New("unknown theme " + choice)
}

// validateThemeDocument checks an imported or submitted theme.
func validateThemeDocument(doc *ThemeDocument) error {
	if doc.Schema != "" && doc.Schema != ThemeSchema {
		return errors.New("unsupported schema " + doc.Schema + ", expected " + ThemeSchema)
	}
	if strings.TrimSpace(doc.Name) == "" {
		return errors.New("a name is required")
	}
	if len([]rune(doc.Name)) > 64 {
		return errors.New("name must be at most 64 characters")
	}
	if doc.Base != ThemeBaseDark && doc.Base != ThemeBaseLight {
		return errors.New("base must be dark or light")
	}
	if len(doc.Vars) > len(themeTokens) {
		return errors.New("too many variables")
	}

	for name, value := range doc.Vars {
		if _, ok := themeTokens[name]; !ok {
			return errors.New("unknown variable " + name)
		}
		if err := validateThemeValue(name, value); err != nil {
			return err
		}
	}
	return nil
}

// validateThemeValue accepts only colours and lengths.
func validateThemeValue(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New(name + " has an empty value")
	}
	if len(value) > 64 {
		return errors.New(name + " value is too long")
	}

	if strings.HasPrefix(name, "--radius") {
		if !lengthPattern.MatchString(value) {
			return errors.New(name + " must be a length such as 12px")
		}
		return nil
	}

	if colorPattern.MatchString(value) ||
		rgbPattern.MatchString(value) ||
		hslPattern.MatchString(value) {
		return nil
	}
	return errors.New(name + " must be a colour such as #7c5cff, rgb(...) or hsl(...)")
}

// --- handlers ---

// handleListThemes serves GET /themes. It needs no authentication: the panel
// reads it before a session exists, and the list carries nothing private.
func (a *API) handleListThemes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, listResponse[BuiltinTheme]{Items: builtinThemes})
}

// handleListCustomThemes serves GET /users/me/themes.
func (a *API) handleListCustomThemes(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFrom(r.Context())

	themes, err := a.store.CustomThemes.ListByUser(r.Context(), principal.UserID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not list themes")
		return
	}

	items := make([]customThemeResponse, 0, len(themes))
	for _, t := range themes {
		items = append(items, toCustomThemeResponse(t))
	}
	writeJSON(w, http.StatusOK, listResponse[customThemeResponse]{Items: items})
}

// handleCreateCustomTheme serves POST /users/me/themes, which doubles as
// theme import.
func (a *API) handleCreateCustomTheme(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFrom(r.Context())

	var doc ThemeDocument
	if !decodeBody(w, r, &doc) {
		return
	}
	if doc.Base == "" {
		doc.Base = ThemeBaseDark
	}
	if err := validateThemeDocument(&doc); err != nil {
		writeFieldError(w, "theme", err.Error())
		return
	}

	theme := &store.CustomTheme{
		UserID: principal.UserID, Name: doc.Name, Base: doc.Base, Vars: doc.Vars,
	}
	if err := a.store.CustomThemes.Create(r.Context(), theme); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not save the theme")
		return
	}

	a.audit(r, principal.UserID, "theme.create", theme.ID, theme.Name)
	writeJSON(w, http.StatusCreated, toCustomThemeResponse(theme))
}

// handlePatchCustomTheme serves PATCH /users/me/themes/{tid}.
func (a *API) handlePatchCustomTheme(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFrom(r.Context())

	theme, ok := a.ownedTheme(w, r, principal)
	if !ok {
		return
	}

	doc := toThemeDocument(theme)
	if !decodeBody(w, r, &doc) {
		return
	}
	if err := validateThemeDocument(&doc); err != nil {
		writeFieldError(w, "theme", err.Error())
		return
	}

	theme.Name, theme.Base, theme.Vars = doc.Name, doc.Base, doc.Vars
	if err := a.store.CustomThemes.Update(r.Context(), theme); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not update the theme")
		return
	}

	a.audit(r, principal.UserID, "theme.update", theme.ID, theme.Name)
	writeJSON(w, http.StatusOK, toCustomThemeResponse(theme))
}

// handleDeleteCustomTheme serves DELETE /users/me/themes/{tid}. If the theme
// was the active one, the profile falls back to its base so the panel does not
// end up pointing at a theme that no longer exists.
func (a *API) handleDeleteCustomTheme(w http.ResponseWriter, r *http.Request) {
	principal, _ := principalFrom(r.Context())

	theme, ok := a.ownedTheme(w, r, principal)
	if !ok {
		return
	}

	if err := a.store.CustomThemes.Delete(r.Context(), theme.ID); err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, "could not delete the theme")
		return
	}

	user, err := a.store.Users.GetByID(r.Context(), principal.UserID)
	if err == nil && user.Theme == customThemePrefix+theme.ID {
		if err := a.store.Users.SetTheme(r.Context(), user.ID, theme.Base); err != nil {
			a.log.Warn("resetting the active theme failed", "error", err)
		}
	}

	a.audit(r, principal.UserID, "theme.delete", theme.ID, "")
	w.WriteHeader(http.StatusNoContent)
}

// ownedTheme loads the theme named in the path and checks ownership.
func (a *API) ownedTheme(w http.ResponseWriter, r *http.Request, principal *Principal) (*store.CustomTheme, bool) {
	theme, err := a.store.CustomThemes.GetByID(r.Context(), r.PathValue("tid"))
	if err != nil || theme.UserID != principal.UserID {
		writeError(w, http.StatusNotFound, CodeServerNotFound, "theme not found")
		return nil, false
	}
	return theme, true
}
