package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "mirocraft.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

// envMap builds a lookupFunc over a fixed map, so tests never touch the real
// environment and can run in parallel.
func envMap(vars map[string]string) lookupFunc {
	return func(key string) (string, bool) {
		v, ok := vars[key]
		return v, ok
	}
}

func TestDefaultIsValid(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("the built-in defaults do not validate: %v", err)
	}
	if cfg.Addr == "" || cfg.DataDir == "" {
		t.Fatal("defaults leave addr or data_dir empty")
	}
}

func TestLoadWithoutFileUsesDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	if cfg.Addr != Default().Addr {
		t.Fatalf("addr = %q, want the default %q", cfg.Addr, Default().Addr)
	}
}

func TestLoadReadsYAML(t *testing.T) {
	path := writeConfig(t, `
addr: "127.0.0.1:9000"
data_dir: "/srv/mirocraft"
log:
  level: debug
  format: json
runner:
  type: process
  stop_timeout: 90s
console:
  buffer_lines: 250
  ticket_ttl: 15s
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Addr != "127.0.0.1:9000" {
		t.Errorf("addr = %q", cfg.Addr)
	}
	if cfg.DataDir != "/srv/mirocraft" {
		t.Errorf("data_dir = %q", cfg.DataDir)
	}
	if cfg.Log.Level != "debug" || cfg.Log.Format != FormatJSON {
		t.Errorf("log = %+v", cfg.Log)
	}
	if cfg.Runner.Type != RunnerProcess || cfg.Runner.StopTimeout != 90*time.Second {
		t.Errorf("runner = %+v", cfg.Runner)
	}
	if cfg.Console.BufferLines != 250 || cfg.Console.TicketTTL != 15*time.Second {
		t.Errorf("console = %+v", cfg.Console)
	}
}

// A partial file must leave everything it does not mention at its default.
func TestLoadPartialYAMLKeepsDefaults(t *testing.T) {
	path := writeConfig(t, "addr: \":1234\"\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":1234" {
		t.Errorf("addr = %q, want :1234", cfg.Addr)
	}
	if cfg.DataDir != Default().DataDir {
		t.Errorf("data_dir = %q, want the default %q", cfg.DataDir, Default().DataDir)
	}
	if cfg.Console.BufferLines != Default().Console.BufferLines {
		t.Errorf("console.buffer_lines = %d, want the default %d",
			cfg.Console.BufferLines, Default().Console.BufferLines)
	}
}

// A misspelled key must fail loudly instead of silently doing nothing.
func TestLoadRejectsUnknownKeys(t *testing.T) {
	path := writeConfig(t, "adress: \":1234\"\n") //nolint:misspell // the typo is the input under test

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load accepted an unknown key")
	}
	if !strings.Contains(err.Error(), "adress") { //nolint:misspell // matching the typo above
		t.Fatalf("error %q does not name the offending key", err)
	}
}

// An explicitly given path that does not exist is an error, not a fallback to
// defaults: the operator meant those settings to apply.
func TestLoadMissingFileIsAnError(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err == nil {
		t.Fatal("Load accepted a missing config file")
	}
}

func TestLoadRejectsMalformedYAML(t *testing.T) {
	path := writeConfig(t, "addr: [this is not a string\n")

	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted malformed yaml")
	}
}

func TestEnvOverridesFile(t *testing.T) {
	cfg := Default()
	cfg.Addr = ":8080"
	cfg.Log.Level = "info"

	err := cfg.applyEnv(envMap(map[string]string{
		EnvPrefix + "ADDR":      ":9999",
		EnvPrefix + "LOG_LEVEL": "debug",
	}))
	if err != nil {
		t.Fatalf("applyEnv: %v", err)
	}

	if cfg.Addr != ":9999" {
		t.Errorf("addr = %q, want the environment value", cfg.Addr)
	}
	if cfg.Log.Level != "debug" {
		t.Errorf("log.level = %q, want the environment value", cfg.Log.Level)
	}
	if cfg.DataDir != Default().DataDir {
		t.Errorf("data_dir = %q, want it untouched", cfg.DataDir)
	}
}

func TestEnvParsesDurationsAndNumbers(t *testing.T) {
	cfg := Default()

	err := cfg.applyEnv(envMap(map[string]string{
		EnvPrefix + "RUNNER_STOP_TIMEOUT":  "2m",
		EnvPrefix + "CONSOLE_TICKET_TTL":   "45s",
		EnvPrefix + "CONSOLE_BUFFER_LINES": "42",
	}))
	if err != nil {
		t.Fatalf("applyEnv: %v", err)
	}

	if cfg.Runner.StopTimeout != 2*time.Minute {
		t.Errorf("runner.stop_timeout = %v", cfg.Runner.StopTimeout)
	}
	if cfg.Console.TicketTTL != 45*time.Second {
		t.Errorf("console.ticket_ttl = %v", cfg.Console.TicketTTL)
	}
	if cfg.Console.BufferLines != 42 {
		t.Errorf("console.buffer_lines = %d", cfg.Console.BufferLines)
	}
}

func TestEnvRejectsUnparseableValues(t *testing.T) {
	tests := []struct {
		name string
		vars map[string]string
	}{
		{"duration", map[string]string{EnvPrefix + "RUNNER_STOP_TIMEOUT": "soon"}},
		{"number", map[string]string{EnvPrefix + "CONSOLE_BUFFER_LINES": "many"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			if err := cfg.applyEnv(envMap(tc.vars)); err == nil {
				t.Fatal("applyEnv accepted an unparseable value")
			}
		})
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"empty addr", func(c *Config) { c.Addr = "" }, "addr"},
		{"blank addr", func(c *Config) { c.Addr = "   " }, "addr"},
		{"empty data dir", func(c *Config) { c.DataDir = "" }, "data_dir"},
		{"bad log level", func(c *Config) { c.Log.Level = "verbose" }, "log.level"},
		{"bad log format", func(c *Config) { c.Log.Format = "xml" }, "log.format"},
		{"bad runner type", func(c *Config) { c.Runner.Type = "kubernetes" }, "runner.type"},
		{"zero stop timeout", func(c *Config) { c.Runner.StopTimeout = 0 }, "runner.stop_timeout"},
		{"negative stop timeout", func(c *Config) { c.Runner.StopTimeout = -time.Second }, "runner.stop_timeout"},
		{"zero buffer", func(c *Config) { c.Console.BufferLines = 0 }, "console.buffer_lines"},
		{"zero ticket ttl", func(c *Config) { c.Console.TicketTTL = 0 }, "console.ticket_ttl"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg)

			err := cfg.Validate()
			if err == nil {
				t.Fatal("Validate accepted an invalid configuration")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// Every problem should be reported at once, so an operator fixes one round of
// errors instead of rediscovering them one restart at a time.
func TestValidateReportsEveryProblem(t *testing.T) {
	cfg := Default()
	cfg.Addr = ""
	cfg.Log.Level = "verbose"
	cfg.Console.BufferLines = -1

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate accepted an invalid configuration")
	}
	for _, want := range []string{"addr", "log.level", "console.buffer_lines"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestParseLevel(t *testing.T) {
	tests := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"warning", slog.LevelWarn},
		{"error", slog.LevelError},
		{"  INFO  ", slog.LevelInfo},
	}

	for _, tc := range tests {
		got, err := ParseLevel(tc.in)
		if err != nil {
			t.Errorf("ParseLevel(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseLevel(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}

	if _, err := ParseLevel("loud"); err == nil {
		t.Error("ParseLevel accepted an unknown level")
	}
}

func TestNewLogger(t *testing.T) {
	for _, format := range []string{FormatText, FormatJSON} {
		cfg := Default()
		cfg.Log.Format = format

		log, err := cfg.NewLogger(os.Stdout)
		if err != nil {
			t.Fatalf("NewLogger(%s): %v", format, err)
		}
		if log == nil {
			t.Fatalf("NewLogger(%s) returned nil", format)
		}
	}

	cfg := Default()
	cfg.Log.Level = "loud"
	if _, err := cfg.NewLogger(os.Stdout); err == nil {
		t.Error("NewLogger accepted an invalid level")
	}
}
