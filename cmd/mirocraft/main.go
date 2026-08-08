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
		addr        = flag.String("addr", ":8080", "address the API listens on")
		dataDir     = flag.String("data-dir", "./data", "directory holding server data")
		logLevel    = flag.String("log-level", "info", "log level: debug, info, warn, error")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("mirocraft", version)
		return nil
	}

	level, err := parseLevel(*logLevel)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)
	api.Version = version

	log.Info("starting mirocraft",
		slog.String("version", version),
		slog.String("addr", *addr),
		slog.String("data_dir", *dataDir))

	// Runner selection is a task 1.5 concern; until DockerRunner exists there
	// is only one implementation to choose.
	processRunner := runner.NewProcessRunner(log)

	// The in-memory auth store is a placeholder until the SQLite store lands
	// in task 1.1. It starts empty, so the API rejects every request until
	// then — deliberately, rather than shipping a default credential.
	store := api.NewMemoryAuth()

	restAPI := api.New(api.Options{
		Auth:    store,
		Servers: store,
		Console: processRunner,
		Logger:  log,
	})

	srv := &http.Server{
		Addr:              *addr,
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
	// WebSocket handlers return, letting the HTTP server drain.
	if err := processRunner.Shutdown(shutdownCtx); err != nil {
		log.Error("shutting down runner failed", slog.String("error", err.Error()))
	}
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down http server: %w", err)
	}

	log.Info("stopped")
	return nil
}

func parseLevel(name string) (slog.Level, error) {
	switch name {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q", name)
	}
}
