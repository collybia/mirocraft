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
	"strings"
	"syscall"
	"time"

	"github.com/collybia/mirocraft/internal/api"
	"github.com/collybia/mirocraft/internal/backup"
	"github.com/collybia/mirocraft/internal/catalog"
	"github.com/collybia/mirocraft/internal/certs"
	"github.com/collybia/mirocraft/internal/config"
	"github.com/collybia/mirocraft/internal/core"
	"github.com/collybia/mirocraft/internal/daemon"
	"github.com/collybia/mirocraft/internal/dns"
	"github.com/collybia/mirocraft/internal/events"
	"github.com/collybia/mirocraft/internal/java"
	"github.com/collybia/mirocraft/internal/php"
	"github.com/collybia/mirocraft/internal/runner"
	"github.com/collybia/mirocraft/internal/store"
	"github.com/collybia/mirocraft/web"
)

// version is stamped at build time with -ldflags.
var version = "dev"

func main() {
	// Under the Windows service control manager this hands control to it and
	// calls run with a context the manager can cancel. Everywhere else, and in
	// a console on Windows, it just calls run.
	if err := runUnderServiceManager(run); err != nil {
		fmt.Fprintln(os.Stderr, "mirocraft:", err)
		os.Exit(1)
	}
}

func run(parent context.Context) error {
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

	selected, err := selectRunner(context.Background(), cfg, log)
	if err != nil {
		return err
	}

	// Core downloads and Java runtimes share the data directory, so a rebuilt
	// server or a second one on the same version costs nothing.
	cores := core.DefaultRegistry(nil)
	downloader := core.NewDownloader(filepath.Join(cfg.DataDir, "cache", "cores"), log)
	javaMgr := java.NewManager(filepath.Join(cfg.DataDir, "java"), log)
	provisioner := daemon.NewProvisioner(cores, downloader, javaMgr, log)
	// A separate manager and a separate directory for compilers: a server runs
	// on a JRE, and the JRE has no javac. Only a core that is built here ever
	// causes this download.
	jdkMgr := java.NewManager(filepath.Join(cfg.DataDir, "jdk"), log)
	jdkMgr.Image = java.ImageJDK
	provisioner.JDK = jdkMgr
	// PocketMine runs on PHP, and on its own PHP: a distribution's refuses the
	// phar over a missing extension.
	provisioner.PHP = php.NewManager(filepath.Join(cfg.DataDir, "php"), log)
	// A container brings its own Java, so downloading 110 MB of JRE onto the
	// host to run a server that will never touch it is pure waste.
	provisioner.SkipHostJava = selected.docker != nil
	if selected.docker != nil {
		// The server runs in a Linux container whatever this host is, and
		// Forge's argument files differ between the two. Deriving it from the
		// host would hand a Windows argument file to a Linux container.
		provisioner.TargetOS = core.TargetLinux
	}
	backups := backup.NewManager(filepath.Join(cfg.DataDir, "backups"), log)

	// The add-on catalogue identifies itself to Modrinth with this build's
	// version: their guidelines ask for a contactable agent, and an anonymous
	// client is the first thing rate-limited when someone abuses the API.
	catalog.UserAgent = "mirocraft/" + version + " (+https://github.com/collybia/mirocraft)"
	addons := catalog.New(nil)

	// DNS is optional in full: a panel reached by address needs none of it,
	// which is the installer's third mode.
	var (
		dnsProvider dns.Provider
		dnsWatcher  *dns.Watcher
	)
	if cfg.DNS.Enabled() {
		dnsProvider, err = dns.NewRegistry().Build(cfg.DNS.Provider, dns.Config{
			Zone: cfg.DNS.Zone, Token: cfg.DNS.Token, TTL: cfg.DNS.TTL,
		})
		if err != nil {
			// Fatal rather than a warning: an operator who configured a name
			// and got a panel reachable only by address would have no way to
			// tell that from the name simply not having propagated yet.
			return err
		}
		dnsWatcher = dns.NewWatcher(dnsProvider, cfg.DNS.Sub, log)
		dnsWatcher.Interval = cfg.DNS.CheckInterval

		log.Info("dns configured",
			slog.String("provider", dnsProvider.ID()),
			slog.String("name", dns.FQDN(cfg.DNS.Sub, dnsProvider.Zone())),
			slog.Bool("srv", dnsProvider.Capabilities().SRV))
		if !dnsProvider.Capabilities().SRV {
			log.Warn("this provider cannot publish SRV records, so players must " +
				"include the port for any server not on 25565")
		}
	}

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

	// The proxies need the server records: what sits behind a proxy, and which
	// proxy a server is behind. Set here rather than where the provisioner is
	// built, because the store is opened after it.
	provisioner.Servers = db.Servers

	if err := bootstrapAdmin(context.Background(), db, cfg.DataDir, log); err != nil {
		return err
	}

	// Runtimes installed by an earlier run are found on disk, so a restart
	// does not re-download what is already there.
	javaMgr.Scan(context.Background())

	// A container survives the daemon that made it, so a restarted daemon has
	// to find the servers it left running rather than assume they are gone.
	if selected.docker != nil {
		if err := selected.docker.Adopt(context.Background()); err != nil {
			log.Warn("adopting existing containers failed", slog.String("error", err.Error()))
		}
	}

	// The certificate comes last of the startup pieces because it may depend
	// on the DNS provider: the dns-01 challenge publishes through it.
	certManager, err := buildCertManager(cfg, dnsProvider, log)
	if err != nil {
		return err
	}

	// The bots run in this process and reach the API over the loopback
	// address, so they are prepared before the API is built and handed to it:
	// saving a token in the panel has to reach something that can act on it.
	scheme := "http"
	if certManager != nil && certManager.Enabled() {
		scheme = "https"
	}
	botSupervisor := startBots(parent, db,
		cfg.Addr, publicPanelURL(scheme, cfg.Addr, cfg.TLS.Domain), log)

	restAPI := api.New(api.Options{
		Store:       db,
		Console:     selected.runner,
		Lifecycle:   selected.runner,
		Provisioner: provisioner,
		Backups:     backups,
		Cores:       cores,
		Catalog:     addons,
		DNS:         dnsPublisher(dnsProvider),
		DNSWatcher:  dnsWatcher,
		Certs:       certStatus(certManager),
		Bots:        botSupervisorOrNil(botSupervisor),
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

	// Derived from the caller's context, so a service stop and a Ctrl+C both
	// end up here: the service manager cancels the parent, and the signals are
	// what an operator running it in a terminal sends.
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Obtained before the listener opens: serving HTTPS with no certificate
	// answers every request with a handshake failure, which looks like the
	// panel being down rather than like a certificate problem.
	var httpChallenge *http.Server
	if certManager != nil && certManager.Enabled() {
		if handler := certManager.HTTPChallengeHandler(); handler != nil {
			httpChallenge = &http.Server{
				Addr:              httpChallengeAddr(cfg),
				Handler:           handler,
				ReadHeaderTimeout: 10 * time.Second,
			}
			go func() {
				if err := httpChallenge.ListenAndServe(); err != nil &&
					!errors.Is(err, http.ErrServerClosed) {
					// Not fatal by itself, but the certificate cannot be
					// obtained without it, so it is worth saying loudly.
					log.Error("the http-01 challenge listener failed; a certificate "+
						"cannot be obtained this way",
						slog.String("addr", httpChallenge.Addr), slog.String("error", err.Error()))
				}
			}()
		}

		if err := certManager.Start(ctx); err != nil {
			return fmt.Errorf("obtaining a certificate: %w", err)
		}
		srv.TLSConfig = certManager.TLSConfig()

		status := certManager.Status()
		log.Info("serving https",
			slog.String("mode", status.Mode), slog.String("domain", status.Domain),
			slog.Bool("trusted", status.Trusted))
		if !status.Trusted {
			log.Warn("the certificate is self-signed, so browsers will warn about it; " +
				"the panel says so too")
		}
	}

	// Scheduled backups tick rather than sleeping to the next due time, so a
	// schedule added while the daemon runs is picked up without a restart.
	go restAPI.RunBackupSchedules(ctx, time.Minute)
	// Action chains tick on the same cadence and for the same reason.
	go restAPI.RunSchedules(ctx, time.Minute)

	// The address record is republished when the connection's address moves,
	// which on a home line happens on every reconnect.
	if dnsWatcher != nil {
		go dnsWatcher.Run(ctx)
	}

	// Webhooks read the same bus the panel's event socket does, so a delivery
	// carries exactly what a watching browser saw.
	dispatcher := events.NewDispatcher(api.WebhookSource(db), db.Webhooks, log)
	dispatcher.AllowPrivateHosts = cfg.Webhooks.AllowPrivateHosts
	go dispatcher.Run(ctx, restAPI.Events())

	errCh := make(chan error, 1)
	go func() {
		var err error
		if srv.TLSConfig != nil {
			// The certificate comes from the manager, so no files are named
			// here.
			err = srv.ListenAndServeTLS("", "")
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
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
	if err := selected.runner.Shutdown(shutdownCtx); err != nil {
		log.Error("shutting down runner failed", slog.String("error", err.Error()))
	}
	if botSupervisor != nil {
		botSupervisor.Shutdown(shutdownCtx)
	}
	if httpChallenge != nil {
		_ = httpChallenge.Shutdown(shutdownCtx)
	}
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutting down http server: %w", err)
	}

	log.Info("stopped")
	return nil
}

// buildCertManager assembles the certificate manager from the configuration.
func buildCertManager(cfg config.Config, provider dns.Provider, log *slog.Logger) (*certs.Manager, error) {
	if !cfg.TLS.Enabled() {
		return nil, nil
	}

	domain := strings.TrimSpace(cfg.TLS.Domain)
	if domain == "" {
		// The name the panel already publishes is the one a browser will use,
		// so repeating it in two places is a chance for them to disagree.
		domain = dns.FQDN(cfg.DNS.Sub, cfg.DNS.Zone)
	}

	var solver certs.DNSSolver
	if provider != nil {
		solver = provider
	}

	return certs.New(certs.Config{
		Mode:         cfg.TLS.Mode,
		Domain:       domain,
		Email:        cfg.TLS.Email,
		Challenge:    cfg.TLS.Challenge,
		DirectoryURL: cfg.TLS.DirectoryURL,
		Dir:          filepath.Join(cfg.DataDir, "certs"),
		AcceptTOS:    cfg.TLS.AcceptTOS,
	}, solver, log)
}

// httpChallengeAddr is where the HTTP-01 challenge is answered.
//
// Port 80 by default because the protocol says so: the authority fetches the
// token over plain HTTP on 80 and will not follow a redirect to another port.
func httpChallengeAddr(cfg config.Config) string {
	if addr := strings.TrimSpace(cfg.TLS.HTTPAddr); addr != "" {
		return addr
	}
	return ":80"
}

// certStatus hands the API a status reporter, or nothing when TLS is off.
//
// A typed nil in an interface is not nil, so a plain assignment would give the
// API a non-nil field wrapping a nil manager, and the status endpoint would
// panic on the first request.
func certStatus(m *certs.Manager) api.CertStatus {
	if m == nil {
		return nil
	}
	return m
}

// dnsPublisher hands the API a publisher, or nothing when DNS is off.
//
// A typed nil in an interface is not nil, so a plain assignment would give the
// API a non-nil DNS field wrapping a nil provider — and every publish would
// panic on the first server created.
func dnsPublisher(provider dns.Provider) api.DNSPublisher {
	if provider == nil {
		return nil
	}
	return provider
}

// chosenRunner is the runner the daemon will use, plus the Docker one when
// that is what was chosen — a few startup steps are Docker-specific and the
// Runner interface deliberately does not know about them.
type chosenRunner struct {
	runner runner.Runner
	docker *runner.DockerRunner
}

// selectRunner picks a runner according to the configuration.
//
// auto prefers Docker and falls back to processes, because Docker gives a
// server a real memory limit and a runtime that need not be installed on the
// host — but a VPS without Docker is a supported install, not an error.
//
// An explicit choice is honoured strictly: an operator who wrote
// runner.type: docker and got processes instead would be running a
// configuration they did not ask for and would have no way to notice.
func selectRunner(ctx context.Context, cfg config.Config, log *slog.Logger) (chosenRunner, error) {
	tryDocker := func() (*runner.DockerRunner, error) {
		dockerRunner, err := runner.NewDockerRunner("", log)
		if err != nil {
			return nil, err
		}
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := dockerRunner.Available(probeCtx); err != nil {
			return nil, err
		}
		return dockerRunner, nil
	}

	switch cfg.Runner.Type {
	case config.RunnerProcess:
		log.Info("runner selected", slog.String("runner", "process"))
		return chosenRunner{runner: runner.NewProcessRunner(log)}, nil

	case config.RunnerDocker:
		dockerRunner, err := tryDocker()
		if err != nil {
			return chosenRunner{}, fmt.Errorf(
				"runner.type is docker but the Docker daemon is not usable: %w", err)
		}
		log.Info("runner selected", slog.String("runner", "docker"),
			slog.String("host", dockerRunner.Client().Host()))
		return chosenRunner{runner: dockerRunner, docker: dockerRunner}, nil

	default: // auto
		dockerRunner, err := tryDocker()
		if err != nil {
			log.Info("runner selected", slog.String("runner", "process"),
				slog.String("docker", err.Error()))
			return chosenRunner{runner: runner.NewProcessRunner(log)}, nil
		}
		log.Info("runner selected", slog.String("runner", "docker"),
			slog.String("host", dockerRunner.Client().Host()))
		return chosenRunner{runner: dockerRunner, docker: dockerRunner}, nil
	}
}

// rootHandler routes /api/v1 to the API and everything else to the embedded
// panel, so both live on one origin and one port.
func rootHandler(apiHandler http.Handler, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/v1/", apiHandler)

	// docs/ROADMAP.md promises the documentation at /api/docs, but every route
	// lives under the version prefix. Without this the path would fall through
	// to the panel and answer 200 with the SPA, which reads as "the docs are
	// broken" rather than "wrong address".
	mux.HandleFunc("/api/docs", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/api/v1/docs", http.StatusMovedPermanently)
	})

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
