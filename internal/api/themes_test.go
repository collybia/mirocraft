package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestListBuiltinThemes(t *testing.T) {
	e := newTestEnv(t)

	// The panel reads this before a session exists, so it must not require a
	// token.
	resp := e.do(http.MethodGet, "/api/v1/themes", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	body := decodeJSON[listResponse[BuiltinTheme]](t, resp)
	want := map[string]string{
		"dark": ThemeBaseDark, "light": ThemeBaseLight, "midnight": ThemeBaseDark,
		"grass": ThemeBaseDark, "nether": ThemeBaseDark,
	}
	if len(body.Items) != len(want) {
		t.Fatalf("listed %d themes, want %d", len(body.Items), len(want))
	}
	for _, theme := range body.Items {
		base, known := want[theme.ID]
		if !known {
			t.Errorf("unexpected theme %q", theme.ID)
			continue
		}
		if theme.Base != base {
			t.Errorf("theme %q has base %q, want %q", theme.ID, theme.Base, base)
		}
		if theme.Name == "" || theme.Preview.BG == "" || theme.Preview.Accent == "" {
			t.Errorf("theme %q is missing display metadata: %+v", theme.ID, theme)
		}
	}
}

func TestValidateThemeChoice(t *testing.T) {
	valid := []string{ThemeSystem, "dark", "light", "midnight", "grass", "nether", "custom:01ABC"}
	for _, choice := range valid {
		if err := validateThemeChoice(choice); err != nil {
			t.Errorf("validateThemeChoice(%q) = %v, want nil", choice, err)
		}
	}

	invalid := []string{"", "solarized", "custom:", "Dark", "system "}
	for _, choice := range invalid {
		if err := validateThemeChoice(choice); err == nil {
			t.Errorf("validateThemeChoice(%q) accepted an invalid choice", choice)
		}
	}
}

// The whitelist is the security boundary for imported themes: anything that
// could carry a URL or escape into arbitrary CSS must be refused.
func TestValidateThemeDocumentRejectsCSSInjection(t *testing.T) {
	tests := []struct {
		name string
		vars map[string]string
	}{
		{"url value", map[string]string{"--bg": "url(https://evil.example/x.png)"}},
		{"expression", map[string]string{"--bg": "expression(alert(1))"}},
		{"semicolon escape", map[string]string{"--bg": "#fff; background: url(x)"}},
		{"closing brace", map[string]string{"--bg": "#fff}"}},
		{"javascript scheme", map[string]string{"--accent": "javascript:alert(1)"}},
		{"var indirection", map[string]string{"--accent": "var(--something-else)"}},
		{"unknown variable", map[string]string{"--evil": "#ffffff"}},
		{"unlisted css property", map[string]string{"background-image": "url(x)"}},
		{"radius given a colour", map[string]string{"--radius": "#ffffff"}},
		{"colour given a length", map[string]string{"--accent": "12px"}},
		{"empty value", map[string]string{"--accent": ""}},
		{"absurdly long value", map[string]string{"--accent": "#" + strings.Repeat("f", 200)}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := &ThemeDocument{Schema: ThemeSchema, Name: "x", Base: ThemeBaseDark, Vars: tc.vars}
			if err := validateThemeDocument(doc); err == nil {
				t.Fatalf("validateThemeDocument accepted %v", tc.vars)
			}
		})
	}
}

func TestValidateThemeDocumentAcceptsValidValues(t *testing.T) {
	doc := &ThemeDocument{
		Schema: ThemeSchema, Name: "Purple", Base: ThemeBaseDark,
		Vars: map[string]string{
			"--accent":        "#7c5cff",
			"--accent-hover":  "#6a4ce0",
			"--bg":            "rgb(20, 20, 24)",
			"--text":          "rgba(230, 232, 238, 0.9)",
			"--border":        "hsl(220, 10%, 30%)",
			"--radius":        "12px",
			"--radius-sm":     "0.5rem",
			"--console-error": "#ff6b6b",
		},
	}
	if err := validateThemeDocument(doc); err != nil {
		t.Fatalf("validateThemeDocument rejected a valid theme: %v", err)
	}
}

func TestValidateThemeDocumentMetadata(t *testing.T) {
	tests := []struct {
		name string
		doc  ThemeDocument
	}{
		{"wrong schema", ThemeDocument{Schema: "someone.else/v9", Name: "x", Base: ThemeBaseDark}},
		{"no name", ThemeDocument{Schema: ThemeSchema, Base: ThemeBaseDark}},
		{"blank name", ThemeDocument{Schema: ThemeSchema, Name: "   ", Base: ThemeBaseDark}},
		{"name too long", ThemeDocument{Schema: ThemeSchema, Name: strings.Repeat("a", 65), Base: ThemeBaseDark}},
		{"unknown base", ThemeDocument{Schema: ThemeSchema, Name: "x", Base: "midnight"}},
		{"no base", ThemeDocument{Schema: ThemeSchema, Name: "x"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := tc.doc
			if err := validateThemeDocument(&doc); err == nil {
				t.Fatalf("validateThemeDocument accepted %+v", doc)
			}
		})
	}
}

// --- custom theme endpoints ---

func TestCustomThemeCRUDOverHTTP(t *testing.T) {
	e := newTestEnv(t)

	created := e.do(http.MethodPost, "/api/v1/users/me/themes", ThemeDocument{
		Schema: ThemeSchema, Name: "Purple", Base: ThemeBaseDark,
		Vars: map[string]string{"--accent": "#7c5cff", "--radius": "12px"},
	}, e.token)
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", created.StatusCode)
	}
	theme := decodeJSON[customThemeResponse](t, created)
	if theme.ID == "" {
		t.Fatal("the created theme has no id")
	}
	if theme.Schema != ThemeSchema {
		t.Errorf("schema = %q, want %q", theme.Schema, ThemeSchema)
	}

	listed := e.do(http.MethodGet, "/api/v1/users/me/themes", nil, e.token)
	if listed.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", listed.StatusCode)
	}
	items := decodeJSON[listResponse[customThemeResponse]](t, listed).Items
	if len(items) != 1 || items[0].Vars["--accent"] != "#7c5cff" {
		t.Fatalf("listed themes = %+v", items)
	}

	patched := e.do(http.MethodPatch, "/api/v1/users/me/themes/"+theme.ID, ThemeDocument{
		Schema: ThemeSchema, Name: "Deep Purple", Base: ThemeBaseDark,
		Vars: map[string]string{"--accent": "#5c3cff"},
	}, e.token)
	if patched.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d, want 200", patched.StatusCode)
	}
	if got := decodeJSON[customThemeResponse](t, patched); got.Name != "Deep Purple" {
		t.Fatalf("patched theme = %+v", got)
	}

	deleted := e.do(http.MethodDelete, "/api/v1/users/me/themes/"+theme.ID, nil, e.token)
	if deleted.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", deleted.StatusCode)
	}
	_ = deleted.Body.Close()
}

// Deleting the active theme must not leave the profile pointing at a theme
// that no longer exists.
func TestDeletingTheActiveThemeFallsBackToItsBase(t *testing.T) {
	e := newTestEnv(t)

	created := e.do(http.MethodPost, "/api/v1/users/me/themes", ThemeDocument{
		Schema: ThemeSchema, Name: "Temp", Base: ThemeBaseLight,
		Vars: map[string]string{"--accent": "#7c5cff"},
	}, e.token)
	theme := decodeJSON[customThemeResponse](t, created)

	choice := "custom:" + theme.ID
	applied := e.do(http.MethodPatch, "/api/v1/users/me", patchMeRequest{Theme: &choice}, e.token)
	if applied.StatusCode != http.StatusOK {
		t.Fatalf("applying the theme gave %d, want 200", applied.StatusCode)
	}
	_ = applied.Body.Close()

	removed := e.do(http.MethodDelete, "/api/v1/users/me/themes/"+theme.ID, nil, e.token)
	if removed.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", removed.StatusCode)
	}
	_ = removed.Body.Close()

	user, err := e.db.Users.GetByID(t.Context(), e.user.ID)
	if err != nil {
		t.Fatalf("reading user: %v", err)
	}
	if user.Theme != ThemeBaseLight {
		t.Fatalf("theme after deleting the active one = %q, want the base %q",
			user.Theme, ThemeBaseLight)
	}
}

func TestCustomThemeOfAnotherUserIsHidden(t *testing.T) {
	e := newTestEnv(t)

	created := e.do(http.MethodPost, "/api/v1/users/me/themes", ThemeDocument{
		Schema: ThemeSchema, Name: "Mine", Base: ThemeBaseDark,
		Vars: map[string]string{"--accent": "#7c5cff"},
	}, e.token)
	theme := decodeJSON[customThemeResponse](t, created)

	otherToken := e.mintToken(e.other.ID, []string{ScopeServersRead})

	for _, method := range []string{http.MethodPatch, http.MethodDelete} {
		resp := e.do(method, "/api/v1/users/me/themes/"+theme.ID, ThemeDocument{
			Schema: ThemeSchema, Name: "Hijacked", Base: ThemeBaseDark,
		}, otherToken)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s by another user gave %d, want 404", method, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}

func TestCreateCustomThemeRejectsInjection(t *testing.T) {
	e := newTestEnv(t)

	resp := e.do(http.MethodPost, "/api/v1/users/me/themes", ThemeDocument{
		Schema: ThemeSchema, Name: "Evil", Base: ThemeBaseDark,
		Vars: map[string]string{"--bg": "url(https://evil.example/beacon.png)"},
	}, e.token)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if code := errorCode(t, resp); code != CodeValidationFailed {
		t.Fatalf("error code = %q, want %q", code, CodeValidationFailed)
	}
}
