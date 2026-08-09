package gamefiles

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// The lists a server keeps beside its world.
const (
	WhitelistName = "whitelist.json"
	OpsName       = "ops.json"
	BansName      = "banned-players.json"
	IPBansName    = "banned-ips.json"
)

// ErrInvalidName is returned for anything that is not a usable player name.
var ErrInvalidName = errors.New("not a valid player name")

// playerNamePattern is Mojang's rule: 3 to 16 characters, letters, digits and
// underscores.
//
// Validating this is a security control, not tidiness. Every player action
// becomes a console command, and a "name" containing a space would add
// arguments to that command — `ban bob --reason` is one thing, but
// `whitelist add bob\nop bob` would be another entirely if newlines were not
// separately refused.
var playerNamePattern = regexp.MustCompile(`^[a-zA-Z0-9_]{3,16}$`)

// ValidatePlayerName reports whether a name is safe to put in a command.
func ValidatePlayerName(name string) error {
	if !playerNamePattern.MatchString(name) {
		return fmt.Errorf("%w: %q must be 3 to 16 letters, digits or underscores", ErrInvalidName, name)
	}
	return nil
}

// Player is one entry of the whitelist or the operator list.
type Player struct {
	Name  string `json:"name"`
	UUID  string `json:"uuid,omitempty"`
	Level int    `json:"level,omitempty"`
}

// Ban is one entry of the ban list.
type Ban struct {
	Name    string     `json:"name"`
	UUID    string     `json:"uuid,omitempty"`
	Source  string     `json:"source,omitempty"`
	Reason  string     `json:"reason,omitempty"`
	Created *time.Time `json:"created,omitempty"`
	Expires string     `json:"expires,omitempty"`
}

// The on-disk shapes. Minecraft writes an array of objects in each file.
type rawWhitelistEntry struct {
	UUID string `json:"uuid"`
	Name string `json:"name"`
}

type rawOpEntry struct {
	UUID  string `json:"uuid"`
	Name  string `json:"name"`
	Level int    `json:"level"`
}

type rawBanEntry struct {
	UUID    string `json:"uuid"`
	Name    string `json:"name"`
	Created string `json:"created"`
	Source  string `json:"source"`
	Expires string `json:"expires"`
	Reason  string `json:"reason"`
}

// banTimeLayout is how Minecraft stamps its ban files.
const banTimeLayout = "2006-01-02 15:04:05 -0700"

// LoadWhitelist reads whitelist.json.
func LoadWhitelist(dir string) ([]Player, error) {
	var raw []rawWhitelistEntry
	if err := readJSONList(filepath.Join(dir, WhitelistName), &raw); err != nil {
		return nil, err
	}

	out := make([]Player, 0, len(raw))
	for _, e := range raw {
		out = append(out, Player{Name: e.Name, UUID: e.UUID})
	}
	return out, nil
}

// LoadOps reads ops.json.
func LoadOps(dir string) ([]Player, error) {
	var raw []rawOpEntry
	if err := readJSONList(filepath.Join(dir, OpsName), &raw); err != nil {
		return nil, err
	}

	out := make([]Player, 0, len(raw))
	for _, e := range raw {
		out = append(out, Player{Name: e.Name, UUID: e.UUID, Level: e.Level})
	}
	return out, nil
}

// LoadBans reads banned-players.json.
func LoadBans(dir string) ([]Ban, error) {
	var raw []rawBanEntry
	if err := readJSONList(filepath.Join(dir, BansName), &raw); err != nil {
		return nil, err
	}

	out := make([]Ban, 0, len(raw))
	for _, e := range raw {
		ban := Ban{
			Name: e.Name, UUID: e.UUID, Source: e.Source,
			Reason: e.Reason, Expires: e.Expires,
		}
		if created, err := time.Parse(banTimeLayout, e.Created); err == nil {
			ban.Created = &created
		}
		out = append(out, ban)
	}
	return out, nil
}

// readJSONList reads one of the server's list files.
//
// A missing file means an empty list rather than an error: a server that has
// never started has none of them, and an empty whitelist is exactly what that
// means.
func readJSONList(path string, target any) error {
	// The path is a fixed file name under a server directory the caller owns.
	body, err := os.ReadFile(path) // #nosec G304 -- a known file in the server's directory
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading %s: %w", filepath.Base(path), err)
	}

	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, target); err != nil {
		return fmt.Errorf("parsing %s: %w", filepath.Base(path), err)
	}
	return nil
}
