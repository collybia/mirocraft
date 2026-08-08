package gamefiles

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A real server.properties, complete with the header comments Minecraft
// writes and a key this panel has never heard of.
const sampleProperties = `#Minecraft server properties
#Fri Aug 08 18:39:12 MSK 2026
motd=A Minecraft Server
max-players=20
difficulty=easy
server-port=25565
white-list=false
level-name=world

# an operator's own note
some-mod-setting=whatever they like
`

func TestParseAndRoundTrip(t *testing.T) {
	props, err := ParseProperties(strings.NewReader(sampleProperties))
	if err != nil {
		t.Fatalf("ParseProperties: %v", err)
	}

	if v, _ := props.Get("motd"); v != "A Minecraft Server" {
		t.Errorf("motd = %q", v)
	}
	if v, _ := props.Get("max-players"); v != "20" {
		t.Errorf("max-players = %q", v)
	}

	// Rendering an unchanged file must give back what was read: comments,
	// blank lines, ordering and all. An editor that reformatted the file on
	// every save would make every change look enormous in a diff.
	if got := props.Render(); got != sampleProperties {
		t.Fatalf("the round trip changed the file:\n--- got ---\n%s\n--- want ---\n%s",
			got, sampleProperties)
	}
}

// A modded server invents its own keys, and dropping them would be worse than
// having no editor at all.
func TestUnknownKeysSurviveAnEdit(t *testing.T) {
	props, err := ParseProperties(strings.NewReader(sampleProperties))
	if err != nil {
		t.Fatalf("ParseProperties: %v", err)
	}

	props.Set("motd", "Изменено панелью")

	rendered := props.Render()
	for _, want := range []string{
		"some-mod-setting=whatever they like",
		"#Minecraft server properties",
		"# an operator's own note",
		"motd=Изменено панелью",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the rendered file is missing %q", want)
		}
	}
	if strings.Contains(rendered, "motd=A Minecraft Server") {
		t.Error("the old value survived the edit")
	}
}

func TestSetAppendsANewKey(t *testing.T) {
	props, err := ParseProperties(strings.NewReader(sampleProperties))
	if err != nil {
		t.Fatalf("ParseProperties: %v", err)
	}

	props.Set("view-distance", "12")

	if v, ok := props.Get("view-distance"); !ok || v != "12" {
		t.Fatalf("view-distance = %q (%v)", v, ok)
	}
	if !strings.Contains(props.Render(), "view-distance=12") {
		t.Error("the new key is missing from the rendered file")
	}
}

func TestValidateValue(t *testing.T) {
	valid := map[string]string{
		"max-players":   "50",
		"difficulty":    "hard",
		"pvp":           "true",
		"motd":          "Любой текст",
		"view-distance": "16",
		// Unknown keys pass as text; a modded server's settings are not this
		// panel's business.
		"some-mod-setting": "anything at all",
	}
	for key, value := range valid {
		if err := ValidateValue(key, value); err != nil {
			t.Errorf("ValidateValue(%q, %q) = %v, want nil", key, value, err)
		}
	}

	invalid := map[string]string{
		"max-players":      "many",
		"difficulty":       "impossible",
		"pvp":              "yes",
		"view-distance":    "1",  // below the minimum
		"spawn-protection": "-5", // below the minimum
		"motd":             "two\nlines",
	}
	for key, value := range invalid {
		if err := ValidateValue(key, value); err == nil {
			t.Errorf("ValidateValue(%q, %q) accepted an invalid value", key, value)
		}
	}
}

// The panel owns the port, so changing it here would put the file and the
// panel's own record out of step.
func TestManagedKeys(t *testing.T) {
	for _, key := range []string{"server-port", "server-ip"} {
		if _, managed := ManagedReason(key); !managed {
			t.Errorf("%q should be managed by the panel", key)
		}
	}
	if _, managed := ManagedReason("motd"); managed {
		t.Error("motd should be editable")
	}
}

func TestSchemaIsUsable(t *testing.T) {
	settings := Schema()
	if len(settings) < 20 {
		t.Fatalf("the schema describes only %d settings", len(settings))
	}

	byKey := map[string]Setting{}
	for _, s := range settings {
		if s.Key == "" {
			t.Fatal("a schema entry has no key")
		}
		byKey[s.Key] = s
	}

	if byKey["difficulty"].Kind != KindEnum || len(byKey["difficulty"].Values) != 4 {
		t.Errorf("difficulty = %+v, want an enum of four", byKey["difficulty"])
	}
	if byKey["max-players"].Kind != KindInt || byKey["max-players"].Min == nil {
		t.Errorf("max-players = %+v, want a bounded int", byKey["max-players"])
	}
}

func TestSaveAndLoad(t *testing.T) {
	dir := t.TempDir()

	props, err := ParseProperties(strings.NewReader(sampleProperties))
	if err != nil {
		t.Fatalf("ParseProperties: %v", err)
	}
	props.Set("motd", "Сохранено")

	if err := props.Save(dir); err != nil {
		t.Fatalf("Save: %v", err)
	}

	reloaded, err := LoadProperties(dir)
	if err != nil {
		t.Fatalf("LoadProperties: %v", err)
	}
	if v, _ := reloaded.Get("motd"); v != "Сохранено" {
		t.Fatalf("motd after a round trip = %q", v)
	}
}

// A server that has never started has no properties file, and every key then
// reads as its default. That is a truer answer than an error.
func TestLoadMissingFileIsEmpty(t *testing.T) {
	props, err := LoadProperties(t.TempDir())
	if err != nil {
		t.Fatalf("LoadProperties on an empty directory: %v", err)
	}
	if len(props.All()) != 0 {
		t.Fatalf("got %d values from a directory with no file", len(props.All()))
	}
}

func TestEscaping(t *testing.T) {
	props, err := ParseProperties(strings.NewReader("motd=line\\nbreak\nname=\\u041c\\u0438\\u0440\n"))
	if err != nil {
		t.Fatalf("ParseProperties: %v", err)
	}

	if v, _ := props.Get("motd"); v != "line\nbreak" {
		t.Errorf("escaped newline = %q", v)
	}
	if v, _ := props.Get("name"); v != "Мир" {
		t.Errorf("unicode escape = %q", v)
	}

	// And writing puts the escapes back, so the file stays one line per key.
	if !strings.Contains(props.Render(), `motd=line\nbreak`) {
		t.Errorf("the newline was not re-escaped: %q", props.Render())
	}
}

// --- player names ---

// Every player action becomes a console command, so the name is a security
// boundary rather than a formatting preference.
func TestValidatePlayerName(t *testing.T) {
	valid := []string{"Notch", "jeb_", "Player123", "abc", "sixteen_chars_16"}
	for _, name := range valid {
		if err := ValidatePlayerName(name); err != nil {
			t.Errorf("ValidatePlayerName(%q) = %v, want nil", name, err)
		}
	}

	invalid := []string{
		"",
		"ab",                // too short
		"seventeen_chars_x", // too long
		"has space",         // would add an argument to the command
		"bob\nop bob",       // would inject a second command
		"bob;op bob",
		"bob\rop bob",
		"Юзер", // Mojang names are ASCII
		"bob-dash",
		"../etc",
		"bob\x00",
	}
	for _, name := range invalid {
		if err := ValidatePlayerName(name); !errors.Is(err, ErrInvalidName) {
			t.Errorf("ValidatePlayerName(%q) = %v, want ErrInvalidName", name, err)
		}
	}
}

// --- the JSON lists ---

func TestLoadWhitelistAndOps(t *testing.T) {
	dir := t.TempDir()

	write(t, dir, WhitelistName, `[
		{"uuid":"069a79f4-44e9-4726-a5be-fca90e38aaf5","name":"Notch"},
		{"uuid":"853c80ef-3c37-49fd-aa49-938b674adae6","name":"jeb_"}
	]`)
	write(t, dir, OpsName, `[
		{"uuid":"069a79f4-44e9-4726-a5be-fca90e38aaf5","name":"Notch","level":4,"bypassesPlayerLimit":false}
	]`)

	whitelist, err := LoadWhitelist(dir)
	if err != nil {
		t.Fatalf("LoadWhitelist: %v", err)
	}
	if len(whitelist) != 2 || whitelist[0].Name != "Notch" {
		t.Fatalf("whitelist = %+v", whitelist)
	}
	if whitelist[0].UUID == "" {
		t.Error("the uuid was dropped")
	}

	ops, err := LoadOps(dir)
	if err != nil {
		t.Fatalf("LoadOps: %v", err)
	}
	if len(ops) != 1 || ops[0].Level != 4 {
		t.Fatalf("ops = %+v", ops)
	}
}

func TestLoadBans(t *testing.T) {
	dir := t.TempDir()

	write(t, dir, BansName, `[
		{"uuid":"069a79f4-44e9-4726-a5be-fca90e38aaf5","name":"Griefer",
		 "created":"2026-08-08 18:00:00 +0300","source":"Server",
		 "expires":"forever","reason":"Griefing"}
	]`)

	bans, err := LoadBans(dir)
	if err != nil {
		t.Fatalf("LoadBans: %v", err)
	}
	if len(bans) != 1 {
		t.Fatalf("got %d bans", len(bans))
	}
	if bans[0].Name != "Griefer" || bans[0].Reason != "Griefing" {
		t.Errorf("ban = %+v", bans[0])
	}
	if bans[0].Created == nil {
		t.Error("the ban timestamp was not parsed")
	}
}

// A server that has never started has none of these files, and an empty list
// is exactly what that means.
func TestMissingListsAreEmpty(t *testing.T) {
	dir := t.TempDir()

	if list, err := LoadWhitelist(dir); err != nil || len(list) != 0 {
		t.Errorf("LoadWhitelist = %v (%v)", list, err)
	}
	if list, err := LoadOps(dir); err != nil || len(list) != 0 {
		t.Errorf("LoadOps = %v (%v)", list, err)
	}
	if list, err := LoadBans(dir); err != nil || len(list) != 0 {
		t.Errorf("LoadBans = %v (%v)", list, err)
	}
}

func TestMalformedListIsAnError(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, WhitelistName, "not json at all")

	if _, err := LoadWhitelist(dir); err == nil {
		t.Fatal("LoadWhitelist accepted a file that is not JSON")
	}
}

// An empty file is what a server writes before anyone is added.
func TestEmptyListFileIsEmpty(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, WhitelistName, "")

	list, err := LoadWhitelist(dir)
	if err != nil {
		t.Fatalf("LoadWhitelist: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("got %d entries from an empty file", len(list))
	}
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o640); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}
