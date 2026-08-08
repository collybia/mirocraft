// Package config loads daemon configuration from defaults, a YAML file and
// the environment.
//
// Precedence, lowest to highest: built-in defaults, the YAML file, environment
// variables, then command-line flags applied by the caller. Every layer is
// optional, so a daemon with no config file and no environment still starts
// with a working configuration.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// EnvPrefix is prepended to every environment variable name.
const EnvPrefix = "MIROCRAFT_"

// Runner selection modes.
const (
	RunnerAuto    = "auto"
	RunnerDocker  = "docker"
	RunnerProcess = "process"
)

// Log output formats.
const (
	FormatText = "text"
	FormatJSON = "json"
)

// Config is the full daemon configuration.
type Config struct {
	// Addr is the listen address of the API and the web panel.
	Addr string `yaml:"addr"`
	// DataDir holds server directories, backups and the database.
	DataDir string `yaml:"data_dir"`

	Log      LogConfig     `yaml:"log"`
	Runner   RunnerConfig  `yaml:"runner"`
	Console  ConsoleConfig `yaml:"console"`
	Webhooks WebhookConfig `yaml:"webhooks"`
}

// WebhookConfig configures outbound webhook delivery.
type WebhookConfig struct {
	// AllowPrivateHosts permits delivery to loopback and private addresses.
	//
	// Off by default: a webhook URL is user-supplied and fetched by the
	// daemon, which is the shape of a server-side request forgery — a hook
	// pointing at a cloud metadata endpoint or at the panel's own port turns
	// it into a proxy for its own network. An operator whose bot runs on the
	// same box can turn it on knowingly.
	AllowPrivateHosts bool `yaml:"allow_private_hosts"`
}

// LogConfig configures slog.
type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// RunnerConfig configures how servers are executed.
type RunnerConfig struct {
	// Type is auto, docker or process. auto picks DockerRunner when Docker is
	// reachable and falls back to ProcessRunner.
	Type string `yaml:"type"`
	// StopTimeout is how long a graceful stop may take before the process is
	// killed.
	StopTimeout time.Duration `yaml:"stop_timeout"`
}

// ConsoleConfig configures the console plumbing.
type ConsoleConfig struct {
	// BufferLines is the per-server scrollback kept in memory.
	BufferLines int `yaml:"buffer_lines"`
	// TicketTTL is how long a WebSocket console ticket stays valid.
	TicketTTL time.Duration `yaml:"ticket_ttl"`
}

// Default returns the built-in configuration.
func Default() Config {
	return Config{
		Addr:    ":8080",
		DataDir: "./data",
		Log: LogConfig{
			Level:  "info",
			Format: FormatText,
		},
		Runner: RunnerConfig{
			Type:        RunnerAuto,
			StopTimeout: 60 * time.Second,
		},
		Console: ConsoleConfig{
			BufferLines: 1000,
			TicketTTL:   30 * time.Second,
		},
	}
}

// Load builds a configuration from defaults, the YAML file at path (skipped
// when path is empty) and the environment, in that order.
//
// A path that was given explicitly but does not exist is an error: silently
// ignoring it would start the daemon with settings the operator did not mean.
func Load(path string) (Config, error) {
	cfg := Default()

	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("reading config %s: %w", path, err)
		}
		// KnownFields makes a typo in a key an error rather than a setting
		// that silently does nothing.
		dec := yaml.NewDecoder(strings.NewReader(string(raw)))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil {
			return Config{}, fmt.Errorf("parsing config %s: %w", path, err)
		}
	}

	if err := cfg.applyEnv(os.LookupEnv); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// lookupFunc matches os.LookupEnv and is injectable for tests.
type lookupFunc func(key string) (string, bool)

// applyEnv overlays MIROCRAFT_* variables onto the configuration.
func (c *Config) applyEnv(lookup lookupFunc) error {
	strVars := map[string]*string{
		"ADDR":        &c.Addr,
		"DATA_DIR":    &c.DataDir,
		"LOG_LEVEL":   &c.Log.Level,
		"LOG_FORMAT":  &c.Log.Format,
		"RUNNER_TYPE": &c.Runner.Type,
	}
	for name, target := range strVars {
		if v, ok := lookup(EnvPrefix + name); ok {
			*target = v
		}
	}

	durVars := map[string]*time.Duration{
		"RUNNER_STOP_TIMEOUT": &c.Runner.StopTimeout,
		"CONSOLE_TICKET_TTL":  &c.Console.TicketTTL,
	}
	for name, target := range durVars {
		v, ok := lookup(EnvPrefix + name)
		if !ok {
			continue
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("%s%s: %w", EnvPrefix, name, err)
		}
		*target = d
	}

	if v, ok := lookup(EnvPrefix + "CONSOLE_BUFFER_LINES"); ok {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("%sCONSOLE_BUFFER_LINES: %w", EnvPrefix, err)
		}
		c.Console.BufferLines = n
	}

	return nil
}

// Validate reports configuration that cannot work.
func (c *Config) Validate() error {
	var problems []string

	if strings.TrimSpace(c.Addr) == "" {
		problems = append(problems, "addr must not be empty")
	}
	if strings.TrimSpace(c.DataDir) == "" {
		problems = append(problems, "data_dir must not be empty")
	}
	if _, err := ParseLevel(c.Log.Level); err != nil {
		problems = append(problems, err.Error())
	}
	if c.Log.Format != FormatText && c.Log.Format != FormatJSON {
		problems = append(problems, fmt.Sprintf("log.format %q must be %s or %s",
			c.Log.Format, FormatText, FormatJSON))
	}
	switch c.Runner.Type {
	case RunnerAuto, RunnerDocker, RunnerProcess:
	default:
		problems = append(problems, fmt.Sprintf("runner.type %q must be %s, %s or %s",
			c.Runner.Type, RunnerAuto, RunnerDocker, RunnerProcess))
	}
	if c.Runner.StopTimeout <= 0 {
		problems = append(problems, "runner.stop_timeout must be positive")
	}
	if c.Console.BufferLines <= 0 {
		problems = append(problems, "console.buffer_lines must be positive")
	}
	if c.Console.TicketTTL <= 0 {
		problems = append(problems, "console.ticket_ttl must be positive")
	}

	if len(problems) > 0 {
		return fmt.Errorf("invalid configuration: %s", strings.Join(problems, "; "))
	}
	return nil
}

// ParseLevel maps a level name onto a slog.Level.
func ParseLevel(name string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("log.level %q must be debug, info, warn or error", name)
	}
}

// NewLogger builds the structured logger described by the configuration.
func (c *Config) NewLogger(w *os.File) (*slog.Logger, error) {
	level, err := ParseLevel(c.Log.Level)
	if err != nil {
		return nil, err
	}
	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	switch c.Log.Format {
	case FormatJSON:
		handler = slog.NewJSONHandler(w, opts)
	case FormatText:
		handler = slog.NewTextHandler(w, opts)
	default:
		return nil, errors.New("log.format must be text or json")
	}
	return slog.New(handler), nil
}
