// Package gamefiles reads and writes the files a Minecraft server keeps in
// its own directory: server.properties, and the JSON lists behind the
// whitelist, the operators and the bans.
//
// Reading those files rather than parsing console output is deliberate. The
// console is a human-readable log whose wording changes between versions and
// forks; the files are what the server itself loads.
package gamefiles

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// PropertiesName is the file inside a server directory.
const PropertiesName = "server.properties"

// Setting kinds, as the panel renders them.
const (
	KindString = "string"
	KindInt    = "int"
	KindBool   = "bool"
	KindEnum   = "enum"
)

// Setting describes one key well enough for the panel to render a control
// for it and for the API to validate a change.
type Setting struct {
	Key     string   `json:"key"`
	Kind    string   `json:"kind"`
	Default string   `json:"default,omitempty"`
	Min     *int     `json:"min,omitempty"`
	Max     *int     `json:"max,omitempty"`
	Values  []string `json:"values,omitempty"`
	Note    string   `json:"note,omitempty"`
}

func intp(v int) *int { return &v }

// schema covers the settings an operator actually changes. It is not
// exhaustive on purpose: a modded or forked server invents its own keys, and
// anything not listed is handled as free-form text rather than refused.
var schema = map[string]Setting{
	"motd":                              {Kind: KindString, Default: "A Minecraft Server", Note: "Строка в списке серверов"},
	"max-players":                       {Kind: KindInt, Default: "20", Min: intp(1), Max: intp(10000)},
	"difficulty":                        {Kind: KindEnum, Default: "easy", Values: []string{"peaceful", "easy", "normal", "hard"}},
	"gamemode":                          {Kind: KindEnum, Default: "survival", Values: []string{"survival", "creative", "adventure", "spectator"}},
	"hardcore":                          {Kind: KindBool, Default: "false"},
	"pvp":                               {Kind: KindBool, Default: "true"},
	"online-mode":                       {Kind: KindBool, Default: "true", Note: "Проверка лицензии; выключайте только осознанно"},
	"white-list":                        {Kind: KindBool, Default: "false"},
	"enforce-whitelist":                 {Kind: KindBool, Default: "false"},
	"spawn-protection":                  {Kind: KindInt, Default: "16", Min: intp(0), Max: intp(1000)},
	"view-distance":                     {Kind: KindInt, Default: "10", Min: intp(2), Max: intp(32)},
	"simulation-distance":               {Kind: KindInt, Default: "10", Min: intp(2), Max: intp(32)},
	"max-world-size":                    {Kind: KindInt, Default: "29999984", Min: intp(1), Max: intp(29999984)},
	"level-name":                        {Kind: KindString, Default: "world"},
	"level-seed":                        {Kind: KindString},
	"level-type":                        {Kind: KindString, Default: "minecraft:normal"},
	"generate-structures":               {Kind: KindBool, Default: "true"},
	"allow-nether":                      {Kind: KindBool, Default: "true"},
	"allow-flight":                      {Kind: KindBool, Default: "false"},
	"spawn-monsters":                    {Kind: KindBool, Default: "true"},
	"spawn-animals":                     {Kind: KindBool, Default: "true"},
	"spawn-npcs":                        {Kind: KindBool, Default: "true"},
	"force-gamemode":                    {Kind: KindBool, Default: "false"},
	"enable-command-block":              {Kind: KindBool, Default: "false"},
	"player-idle-timeout":               {Kind: KindInt, Default: "0", Min: intp(0), Max: intp(1440)},
	"op-permission-level":               {Kind: KindInt, Default: "4", Min: intp(1), Max: intp(4)},
	"function-permission-level":         {Kind: KindInt, Default: "2", Min: intp(1), Max: intp(4)},
	"enable-rcon":                       {Kind: KindBool, Default: "false", Note: "Панель управляет сервером через консоль, RCON не нужен"},
	"enable-query":                      {Kind: KindBool, Default: "false"},
	"enable-status":                     {Kind: KindBool, Default: "true", Note: "Выключение скрывает сервер из списка и ломает счётчик игроков"},
	"hide-online-players":               {Kind: KindBool, Default: "false"},
	"require-resource-pack":             {Kind: KindBool, Default: "false"},
	"resource-pack":                     {Kind: KindString},
	"resource-pack-prompt":              {Kind: KindString},
	"sync-chunk-writes":                 {Kind: KindBool, Default: "true"},
	"entity-broadcast-range-percentage": {Kind: KindInt, Default: "100", Min: intp(10), Max: intp(1000)},
	"network-compression-threshold":     {Kind: KindInt, Default: "256", Min: intp(-1), Max: intp(65535)},
	"max-tick-time":                     {Kind: KindInt, Default: "60000", Min: intp(-1), Max: intp(600000)},
	"max-chained-neighbor-updates":      {Kind: KindInt, Default: "1000000"},
	"rate-limit":                        {Kind: KindInt, Default: "0", Min: intp(0), Max: intp(1000)},
	"text-filtering-config":             {Kind: KindString},
	"initial-enabled-packs":             {Kind: KindString},
	"initial-disabled-packs":            {Kind: KindString},
	"log-ips":                           {Kind: KindBool, Default: "true"},
	"pause-when-empty-seconds":          {Kind: KindInt, Default: "60", Min: intp(-1), Max: intp(86400)},
}

// Managed keys the panel owns. Changing them through the settings API would
// put the file and the panel's own record out of step, so they are refused
// with an explanation rather than silently ignored.
var managed = map[string]string{
	"server-port": "порт задаётся при создании сервера и меняется в его настройках, а не здесь",
	"server-ip":   "адрес прослушивания определяется панелью",
	"rcon.port":   "RCON не используется",
	"query.port":  "порт query определяется панелью",
}

// Properties is a parsed server.properties that remembers everything it did
// not understand.
type Properties struct {
	// lines is the file as read, so writing preserves comments, ordering and
	// any key the panel has never heard of. A settings editor that quietly
	// dropped a modded server's keys would be worse than no editor.
	lines  []propLine
	values map[string]string
}

type propLine struct {
	// raw is the original text for a comment or a blank line.
	raw string
	// key is set for a setting line.
	key string
}

// ParseProperties reads a properties file.
func ParseProperties(r io.Reader) (*Properties, error) {
	p := &Properties{values: make(map[string]string)}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)

	for scanner.Scan() {
		raw := scanner.Text()
		trimmed := strings.TrimSpace(raw)

		// Comments and blanks are carried through untouched.
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "!") {
			p.lines = append(p.lines, propLine{raw: raw})
			continue
		}

		key, value, ok := splitProperty(trimmed)
		if !ok {
			p.lines = append(p.lines, propLine{raw: raw})
			continue
		}

		p.values[key] = value
		p.lines = append(p.lines, propLine{key: key})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading properties: %w", err)
	}
	return p, nil
}

// LoadProperties reads the file from a server directory.
func LoadProperties(dir string) (*Properties, error) {
	// A fixed file name under a server directory the caller owns.
	file, err := os.Open(filepath.Join(dir, PropertiesName)) // #nosec G304 -- a known name in the server's directory
	if err != nil {
		if os.IsNotExist(err) {
			// A server that has never started has no properties file yet.
			// An empty set is a truer answer than an error, since every key
			// then reads as its default.
			return &Properties{values: make(map[string]string)}, nil
		}
		return nil, fmt.Errorf("opening %s: %w", PropertiesName, err)
	}
	defer func() { _ = file.Close() }()

	return ParseProperties(file)
}

// Get returns a value and whether it was present.
func (p *Properties) Get(key string) (string, bool) {
	v, ok := p.values[key]
	return v, ok
}

// Set records a value, appending the key if the file did not have it.
func (p *Properties) Set(key, value string) {
	if _, exists := p.values[key]; !exists {
		p.lines = append(p.lines, propLine{key: key})
	}
	p.values[key] = value
}

// All returns every key and value.
func (p *Properties) All() map[string]string {
	out := make(map[string]string, len(p.values))
	for k, v := range p.values {
		out[k] = v
	}
	return out
}

// Keys returns the keys in file order, then any that were appended.
func (p *Properties) Keys() []string {
	var out []string
	for _, line := range p.lines {
		if line.key != "" {
			out = append(out, line.key)
		}
	}
	return out
}

// Render writes the file back, preserving what was there.
func (p *Properties) Render() string {
	var b strings.Builder
	for _, line := range p.lines {
		if line.key == "" {
			b.WriteString(line.raw)
			b.WriteByte('\n')
			continue
		}
		b.WriteString(line.key)
		b.WriteByte('=')
		b.WriteString(escapeValue(p.values[line.key]))
		b.WriteByte('\n')
	}
	return b.String()
}

// Save writes the file into a server directory.
func (p *Properties) Save(dir string) error {
	path := filepath.Join(dir, PropertiesName)

	// Through a temporary file: a server.properties truncated by an
	// interrupted write is a server that will not start.
	tmp, err := os.CreateTemp(dir, ".properties-*")
	if err != nil {
		return fmt.Errorf("creating a temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()

	if _, err := io.WriteString(tmp, p.Render()); err != nil {
		return fmt.Errorf("writing %s: %w", PropertiesName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", PropertiesName, err)
	}
	if err := os.Chmod(tmpPath, 0o640); err != nil {
		return fmt.Errorf("setting permissions: %w", err)
	}
	return os.Rename(tmpPath, path)
}

// SettingFor returns the schema entry for a key, and whether one exists.
func SettingFor(key string) (Setting, bool) {
	s, ok := schema[key]
	if ok {
		s.Key = key
	}
	return s, ok
}

// Schema returns every described setting, sorted by key.
func Schema() []Setting {
	out := make([]Setting, 0, len(schema))
	for key, s := range schema {
		s.Key = key
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// ManagedReason explains why a key cannot be set through this API, or returns
// false if it can.
func ManagedReason(key string) (string, bool) {
	reason, ok := managed[key]
	return reason, ok
}

// ValidateValue checks a value against the schema. Unknown keys are accepted
// as text, since a modded server's own keys are not this panel's business.
func ValidateValue(key, value string) error {
	setting, known := SettingFor(key)
	if !known {
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("value for %s must not contain line breaks", key)
		}
		return nil
	}

	switch setting.Kind {
	case KindBool:
		if value != "true" && value != "false" {
			return fmt.Errorf("%s must be true or false", key)
		}

	case KindInt:
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("%s must be a whole number", key)
		}
		if setting.Min != nil && n < *setting.Min {
			return fmt.Errorf("%s must be at least %d", key, *setting.Min)
		}
		if setting.Max != nil && n > *setting.Max {
			return fmt.Errorf("%s must be at most %d", key, *setting.Max)
		}

	case KindEnum:
		for _, allowed := range setting.Values {
			if value == allowed {
				return nil
			}
		}
		return fmt.Errorf("%s must be one of: %s", key, strings.Join(setting.Values, ", "))

	default:
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("value for %s must not contain line breaks", key)
		}
	}
	return nil
}

// splitProperty parses one setting line.
//
// The format allows '=' , ':' or whitespace as the separator; Minecraft only
// ever writes '=', but a file edited by hand may use another.
func splitProperty(line string) (key, value string, ok bool) {
	for i, r := range line {
		if r == '=' || r == ':' {
			return strings.TrimSpace(line[:i]), unescapeValue(strings.TrimSpace(line[i+1:])), true
		}
	}
	return "", "", false
}

// unescapeValue turns the escapes Java writes back into their characters.
func unescapeValue(value string) string {
	if !strings.ContainsRune(value, '\\') {
		return value
	}

	var b strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] != '\\' || i+1 >= len(value) {
			b.WriteByte(value[i])
			continue
		}

		i++
		switch value[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'u':
			if i+4 < len(value) {
				if n, err := strconv.ParseUint(value[i+1:i+5], 16, 32); err == nil {
					b.WriteRune(rune(n))
					i += 4
					continue
				}
			}
			b.WriteByte('u')
		default:
			b.WriteByte(value[i])
		}
	}
	return b.String()
}

// escapeValue writes a value the way Java reads it back.
//
// Only the characters that would break the format are escaped. Minecraft
// writes non-ASCII as \uXXXX, but modern servers read UTF-8 fine and a motd
// full of escapes is unreadable when an operator opens the file, so text is
// left as it is.
func escapeValue(value string) string {
	replacer := strings.NewReplacer(
		"\\", "\\\\",
		"\n", "\\n",
		"\r", "\\r",
		"\t", "\\t",
	)
	return replacer.Replace(value)
}
