// Command mirocraft runs the Mirocraft daemon: the server supervisor, the REST
// API and (once task 3.1 lands) the embedded web panel, all in one binary.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/temertika/mirocraft/internal/api"
	"github.com/temertika/mirocraft/internal/config"
	"github.com/temertika/mirocraft/internal/runner"
	"github.com/temertika/mirocraft/internal/store"
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

	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return fmt.Errorf("creating data dir %s: %w", cfg.DataDir, err)
	}

	dbPath := filepath.Join(cfg.DataDir, "mirocraft.db")
	db, err := store.Open(context.Background(), dbPath)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	log.Info("database ready", slog.String("path", dbPath))

	if err := bootstrapAdmin(context.Background(), db, log); err != nil {
		return err
	}

	restAPI := api.New(api.Options{
		Store:       db,
		Console:     processRunner,
		Lifecycle:   processRunner,
		Logger:      log,
		DataDir:     cfg.DataDir,
		TicketTTL:   cfg.Console.TicketTTL,
		StopTimeout: cfg.Runner.StopTimeout,
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

// bootstrapAdmin creates the first administrator on an empty database and
// prints the generated password once.
//
// The password is generated rather than defaulted: a well-known first-run
// credential on a panel that is, by design, reachable from the internet is a
// standing invitation.
func bootstrapAdmin(ctx context.Context, db *store.Store, log *slog.Logger) error {
	count, err := db.Users.Count(ctx)
	if err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	password, err := generatePassword()
	if err != nil {
		return err
	}
	hash, err := store.HashPassword(password)
	if err != nil {
		return err
	}

	admin := &store.User{Email: "admin@localhost", PasswordHash: hash, Role: store.RoleAdmin}
	if err := db.Users.Create(ctx, admin); err != nil {
		return fmt.Errorf("creating the first admin: %w", err)
	}

	// Straight to stdout, not the logger: this must not end up in a log
	// aggregator, and it is shown exactly once.
	// Deliberately without box drawing: aligning a frame around text that
	// mixes Cyrillic and generated tokens breaks in every terminal that
	// disagrees about character width.
	fmt.Println()
	fmt.Println("  ── Первый запуск: создан администратор ──")
	fmt.Println()
	fmt.Println("     Логин:  " + admin.Email)
	fmt.Println("     Пароль: " + password)
	fmt.Println()
	fmt.Println("  Пароль показывается один раз. Сохраните его")
	fmt.Println("  и смените после первого входа.")
	fmt.Println()

	log.Info("first admin account created", slog.String("email", admin.Email))
	return nil
}

// generatePassword returns a random password of roughly 128 bits of entropy.
func generatePassword() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating a password: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
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
