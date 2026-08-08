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

	"github.com/collybia/mirocraft/internal/api"
	"github.com/collybia/mirocraft/internal/backup"
	"github.com/collybia/mirocraft/internal/config"
	"github.com/collybia/mirocraft/internal/core"
	"github.com/collybia/mirocraft/internal/daemon"
	"github.com/collybia/mirocraft/internal/events"
	"github.com/collybia/mirocraft/internal/java"
	"github.com/collybia/mirocraft/internal/runner"
	"github.com/collybia/mirocraft/internal/store"
	"github.com/collybia/mirocraft/web"
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

	// Core downloads and Java runtimes share the data directory, so a rebuilt
	// server or a second one on the same version costs nothing.
	cores := core.DefaultRegistry(nil)
	downloader := core.NewDownloader(filepath.Join(cfg.DataDir, "cache", "cores"), log)
	javaMgr := java.NewManager(filepath.Join(cfg.DataDir, "java"), log)
	provisioner := daemon.NewProvisioner(cores, downloader, javaMgr, log)
	backups := backup.NewManager(filepath.Join(cfg.DataDir, "backups"), log)

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

	if err := bootstrapAdmin(context.Background(), db, cfg.DataDir, log); err != nil {
		return err
	}

	// Runtimes installed by an earlier run are found on disk, so a restart
	// does not re-download what is already there.
	javaMgr.Scan(context.Background())

	restAPI := api.New(api.Options{
		Store:       db,
		Console:     processRunner,
		Lifecycle:   processRunner,
		Provisioner: provisioner,
		Backups:     backups,
		Logger:      log,
		DataDir:     cfg.DataDir,
		TicketTTL:   cfg.Console.TicketTTL,
		StopTimeout: cfg.Runner.StopTimeout,
	})

	// A previous daemon's children outlive it, so any row still claiming to be
	// running describes a process this one cannot manage.
	restAPI.ReconcileServers(context.Background())

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           rootHandler(restAPI.Handler(), log),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: it would cut off console WebSocket streams.
		IdleTimeout: 120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Scheduled backups tick rather than sleeping to the next due time, so a
	// schedule added while the daemon runs is picked up without a restart.
	go restAPI.RunBackupSchedules(ctx, time.Minute)

	// Webhooks read the same bus the panel's event socket does, so a delivery
	// carries exactly what a watching browser saw.
	dispatcher := events.NewDispatcher(api.WebhookSource(db), db.Webhooks, log)
	dispatcher.AllowPrivateHosts = cfg.Webhooks.AllowPrivateHosts
	go dispatcher.Run(ctx, restAPI.Events())

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

// rootHandler routes /api/v1 to the API and everything else to the embedded
// panel, so both live on one origin and one port.
func rootHandler(apiHandler http.Handler, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/v1/", apiHandler)

	panel, err := web.Handler()
	if err != nil {
		// A daemon without a panel is still a working API, so this is a
		// warning rather than a failure to start.
		log.Warn("the web panel is not available", slog.String("reason", err.Error()))
		mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("Панель не собрана. Соберите её: cd web && npm ci && npm run build\n"))
		})
		return mux
	}

	mux.Handle("/", panel)
	log.Info("web panel mounted")
	return mux
}

// bootstrapAdmin creates the first administrator on an empty database and
// prints the generated password once.
//
// The password is generated rather than defaulted: a well-known first-run
// credential on a panel that is, by design, reachable from the internet is a
// standing invitation.
func bootstrapAdmin(ctx context.Context, db *store.Store, dataDir string, log *slog.Logger) error {
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

	// A plain login rather than an address: the daemon sends no mail, and
	// "admin@localhost" only invites the question of which mailbox that is.
	admin := &store.User{Email: "admin", PasswordHash: hash, Role: store.RoleAdmin}
	if err := db.Users.Create(ctx, admin); err != nil {
		return fmt.Errorf("creating the first admin: %w", err)
	}

	// Printed to stdout rather than through the logger, so it is not shipped
	// with structured logs.
	//
	// That is only half the problem: under systemd, stdout IS the journal, so
	// the password lands in it anyway. It is therefore also written to a
	// file readable only by the daemon's own user, and the message says so —
	// an operator who runs the daemon as a service has somewhere to look
	// besides `journalctl`.
	//
	// Deliberately without box drawing: aligning a frame around text that
	// mixes Cyrillic and generated tokens breaks in every terminal that
	// disagrees about character width.
	credentialsPath := filepath.Join(dataDir, "initial-admin.txt")
	body := fmt.Sprintf(
		"login: %s\npassword: %s\n\nСмените пароль после первого входа и удалите этот файл.\n",
		admin.Email, password)
	// 0600: readable only by the user the daemon runs as.
	if err := os.WriteFile(credentialsPath, []byte(body), 0o600); err != nil {
		log.Warn("could not write the initial credentials file",
			slog.String("path", credentialsPath), slog.String("error", err.Error()))
		credentialsPath = ""
	}

	fmt.Println()
	fmt.Println("  ── Первый запуск: создан администратор ──")
	fmt.Println()
	fmt.Println("     Логин:  " + admin.Email)
	fmt.Println("     Пароль: " + password)
	fmt.Println()
	fmt.Println("  Смените пароль после первого входа.")
	if credentialsPath != "" {
		fmt.Println("  Он также записан в " + credentialsPath + " — удалите файл,")
		fmt.Println("  когда сохраните пароль.")
	}
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
