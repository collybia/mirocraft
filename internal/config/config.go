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
	DNS      DNSConfig     `yaml:"dns"`
	TLS      TLSConfig     `yaml:"tls"`
}

// TLSConfig configures how the panel is served over HTTPS.
type TLSConfig struct {
	// Mode is off, acme or self-signed. Off serves plain HTTP, which is the
	// right answer behind a reverse proxy that terminates TLS itself.
	Mode string `yaml:"mode"`
	// Domain the certificate covers. Empty falls back to dns.zone.
	Domain string `yaml:"domain"`
	// Email is the ACME account contact; the authority uses it to warn about
	// expiry.
	Email string `yaml:"email"`
	// Challenge is http-01 or dns-01.
	Challenge string `yaml:"challenge"`
	// DirectoryURL overrides the certificate authority, for staging or tests.
	DirectoryURL string `yaml:"directory_url"`
	// AcceptTOS records that the operator agreed to the authority's terms.
	AcceptTOS bool `yaml:"accept_tos"`
	// HTTPAddr is where the HTTP-01 challenge is answered and plain HTTP is
	// redirected from. Empty means :80.
	HTTPAddr string `yaml:"http_addr"`
}

// Enabled reports whether HTTPS is served.
func (t TLSConfig) Enabled() bool {
	mode := strings.TrimSpace(t.Mode)
	return mode != "" && mode != "off"
}

// DNSConfig configures the name the panel and its servers are reachable by.
//
// Optional in full: a panel reached by IP address needs none of it, which is
// the third installer mode.
type DNSConfig struct {
	// Provider is desec, duckdns or cloudflare. Empty disables DNS entirely.
	Provider string `yaml:"provider"`
	// Zone is the domain: "myname.dedyn.io", "myname" for DuckDNS, or a
	// domain already on Cloudflare.
	Zone string `yaml:"zone"`
	// Token authenticates with the provider.
	//
	// Read from the file rather than the database on purpose: it is an
	// infrastructure credential the operator owns, not something a panel user
	// should be able to read back or change.
	Token string `yaml:"token"`
	// Sub is the name under the zone the panel's own address is published as.
	// Empty publishes the zone itself, which is what a free subdomain wants.
	Sub string `yaml:"sub"`
	// TTL overrides the default; providers raise it to their own floor.
	TTL int `yaml:"ttl"`
	// CheckInterval is how often the public address is re-checked.
	CheckInterval time.Duration `yaml:"check_interval"`
}

// Enabled reports whether DNS is configured.
func (d DNSConfig) Enabled() bool {
	return strings.TrimSpace(d.Provider) != "" && strings.TrimSpace(d.Token) != ""
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
		// The path is what the operator passed on the command line.
		raw, err := os.ReadFile(path) // #nosec G304 -- the operator's own configuration file
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
	// Half-configured DNS is worse than none: the daemon would start, publish
	// nothing and leave the operator wondering why the name does not resolve.
	if strings.TrimSpace(c.DNS.Provider) != "" {
		if strings.TrimSpace(c.DNS.Zone) == "" {
			problems = append(problems, "dns.zone is required when dns.provider is set")
		}
		if strings.TrimSpace(c.DNS.Token) == "" {
			problems = append(problems, "dns.token is required when dns.provider is set")
		}
	}
	if strings.TrimSpace(c.DNS.Provider) == "" && strings.TrimSpace(c.DNS.Token) != "" {
		problems = append(problems, "dns.token is set but dns.provider is not")
	}

	switch strings.TrimSpace(c.TLS.Mode) {
	case "", "off", "self-signed":
	case "acme":
		// The domain can come from dns.zone, so only the combination of both
		// missing is a problem.
		if strings.TrimSpace(c.TLS.Domain) == "" && strings.TrimSpace(c.DNS.Zone) == "" {
			problems = append(problems, "tls.domain is required when tls.mode is acme and dns.zone is not set")
		}
		if !c.TLS.AcceptTOS {
			problems = append(problems,
				"tls.accept_tos must be true to obtain a certificate from a certificate authority")
		}
		switch strings.TrimSpace(c.TLS.Challenge) {
		case "", "http-01":
		case "dns-01":
			if !c.DNS.Enabled() {
				problems = append(problems,
					"tls.challenge dns-01 needs a configured dns.provider to publish the challenge record")
			}
		default:
			problems = append(problems, "tls.challenge must be http-01 or dns-01")
		}
	default:
		problems = append(problems, "tls.mode must be off, acme or self-signed")
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
