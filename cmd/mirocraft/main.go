// Command mirocraft runs the Mirocraft daemon: the server supervisor, the REST
// API and (once task 3.1 lands) the embedded web panel, all in one binary.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/temertika/mirocraft/internal/api"
	"github.com/temertika/mirocraft/internal/config"
	"github.com/temertika/mirocraft/internal/runner"
)

// version is stamped at build time with -ldflags.
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "mirocraft:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "", "path to the configuration file")
		addr        = flag.String("addr", "", "address the API listens on (overrides the config)")
		dataDir     = flag.String("data-dir", "", "directory holding server data (overrides the config)")
		logLevel    = flag.String("log-level", "", "log level: debug, info, warn, error (overrides the config)")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("mirocraft", version)
		return nil
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	// Flags are the last word, above the file and the environment. Only flags
	// the operator actually passed are applied, so an unset flag does not
	// overwrite a configured value with an empty string.
	applyFlagOverrides(&cfg, map[string]*string{
		"addr":      addr,
		"data-dir":  dataDir,
		"log-level": logLevel,
	})
	if err := cfg.Validate(); err != nil {
		return err
	}

	log, err := cfg.NewLogger(os.Stdout)
	if err != nil {
		return err
	}
	slog.SetDefault(log)
	api.Version = version

	log.Info("starting mirocraft",
		slog.String("version", version),
		slog.String("addr", cfg.Addr),
		slog.String("data_dir", cfg.DataDir),
		slog.String("runner", cfg.Runner.Type))

	// Runner selection is a task 1.5 concern; until DockerRunner exists there
	// is only one implementation, so docker and auto both land on it.
	if cfg.Runner.Type == config.RunnerDocker {
		log.Warn("docker runner is not implemented yet, using the process runner")
	}
	processRunner := runner.NewProcessRunner(log)

	// The in-memory auth store is a placeholder until the SQLite store lands
	// in task 1.1. It starts empty, so the API rejects every request until
	// then — deliberately, rather than shipping a default credential.
	store := api.NewMemoryAuth()

	restAPI := api.New(api.Options{
		Auth:      store,
		Servers:   store,
		Console:   processRunner,
		Logger:    log,
		TicketTTL: cfg.Console.TicketTTL,
	})

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           restAPI.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: it would cut off console WebSocket streams.
		IdleTimeout: 120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http server: %w", err)
			return
		}
		errCh <- nil
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		log.Info("shutdown requested, draining")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// The runner goes first so console subscriptions are released and their
	// WebSocket handlers return, letting the HTTP server drain instead of
	// waiting out the shutdown timeout.
	if err := processRunner.Shutdown(shutdownCtx); err != nil {
		log.Error("shutting down runner failed", slog.String("error", err.Error()))
	}
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down http server: %w", err)
	}

	log.Info("stopped")
	return nil
}

// applyFlagOverrides copies the values of flags that were actually set on the
// command line into the configuration.
func applyFlagOverrides(cfg *config.Config, flags map[string]*string) {
	targets := map[string]*string{
		"addr":      &cfg.Addr,
		"data-dir":  &cfg.DataDir,
		"log-level": &cfg.Log.Level,
	}

	flag.Visit(func(f *flag.Flag) {
		source, ok := flags[f.Name]
		if !ok {
			return
		}
		target, ok := targets[f.Name]
		if !ok {
			return
		}
		*target = *source
	})
}
