// Command mirocraft-relay gives home machines a public address.
//
// It runs on a machine that has one — a VPS — and forwards players to a panel
// that dials out to it. Separate from the panel binary because it runs
// somewhere else and does something else: the panel is a control panel, and
// this is a forwarder with no state, no database and no web interface.
//
//	mirocraft-relay --config /etc/mirocraft-relay.yaml
//	mirocraft-relay --add "дом Пети" --port 25565
//
// The second form mints a tunnel: it prints the token once, writes its hash to
// the configuration, and never stores the token itself.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"gopkg.in/yaml.v3"

	"github.com/collybia/mirocraft/internal/relay"
)

// version is set at build time.
var version = "dev"

type config struct {
	// Control is where panels connect, host:port.
	Control string `yaml:"control"`
	TLS     struct {
		Cert string `yaml:"cert"`
		Key  string `yaml:"key"`
	} `yaml:"tls"`
	Tunnels []tunnelConfig `yaml:"tunnels"`
}

type tunnelConfig struct {
	Name string `yaml:"name"`
	// TokenHash is all the relay keeps. The token is shown once, when the
	// tunnel is created, and cannot be recovered — which is the point.
	TokenHash string `yaml:"token_hash"`
	Port      int    `yaml:"port"`
}

func main() {
	var (
		configPath  = flag.String("config", "/etc/mirocraft-relay.yaml", "path to the configuration")
		add         = flag.String("add", "", "create a tunnel with this name and print its token")
		port        = flag.Int("port", 0, "public port for the tunnel being created")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("mirocraft-relay", version)
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if *add != "" {
		if err := addTunnel(*configPath, *add, *port); err != nil {
			fmt.Fprintln(os.Stderr, "mirocraft-relay:", err)
			os.Exit(1)
		}
		return
	}

	cfg, err := load(*configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mirocraft-relay:", err)
		os.Exit(1)
	}
	if len(cfg.Tunnels) == 0 {
		fmt.Fprintln(os.Stderr, "mirocraft-relay: no tunnels configured — see --add")
		os.Exit(1)
	}

	tunnels := make([]relay.Tunnel, 0, len(cfg.Tunnels))
	for _, t := range cfg.Tunnels {
		tunnels = append(tunnels, relay.Tunnel{Name: t.Name, TokenHash: t.TokenHash, Port: t.Port})
	}

	server := relay.NewServer(relay.Config{
		ControlAddr: cfg.Control,
		Tunnels:     tunnels,
		TLS:         relay.TLSConfig{CertFile: cfg.TLS.Cert, KeyFile: cfg.TLS.Key},
		Log:         log,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("starting mirocraft-relay", slog.String("version", version))
	if err := server.Run(ctx); err != nil {
		log.Error("relay stopped", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func load(path string) (config, error) {
	// #nosec G304 -- the path is the operator's own argument
	raw, err := os.ReadFile(path)
	if err != nil {
		return config{}, fmt.Errorf("reading %s: %w", path, err)
	}

	var cfg config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return config{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	if strings.TrimSpace(cfg.Control) == "" {
		cfg.Control = ":7000"
	}
	return cfg, nil
}

// addTunnel writes a new tunnel into the configuration and prints its token.
//
// Printed once and never stored: the relay only ever needs to recognise a
// token, so keeping one would add a file worth stealing and nothing else. An
// operator who loses it creates another tunnel.
func addTunnel(path, name string, port int) error {
	if port < 1 || port > 65535 {
		return errors.New("--port is required and must be a port")
	}

	cfg, err := load(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) && !strings.Contains(err.Error(), "no such file") {
			return err
		}
		cfg = config{Control: ":7000"}
	}

	for _, existing := range cfg.Tunnels {
		if existing.Port == port {
			return fmt.Errorf("port %d already belongs to tunnel %q", port, existing.Name)
		}
	}

	token, err := relay.NewToken()
	if err != nil {
		return err
	}
	cfg.Tunnels = append(cfg.Tunnels, tunnelConfig{
		Name: name, TokenHash: relay.HashToken(token), Port: port,
	})

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("rendering the configuration: %w", err)
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	// 0600: the file holds hashes rather than tokens, but the list of who may
	// connect is nobody else's business either.
	if err := os.WriteFile(path, out, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	fmt.Printf(`
  Туннель «%s» создан.

     Порт для игроков:  %d
     Токен:             %s

  Токен показывается один раз — он не хранится, в файле только его хэш.
  Впишите его в конфигурацию панели на домашней машине:

     relay:
       addr: "этот-сервер:7000"
       token: "%s"

`, name, port, token, token)
	return nil
}
