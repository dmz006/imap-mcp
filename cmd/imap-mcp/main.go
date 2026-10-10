package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/enrichment"
	"github.com/dmz006/imap-mcp/internal/httpauth"
	"github.com/dmz006/imap-mcp/internal/imap"
	"github.com/dmz006/imap-mcp/internal/imap/auth"
	"github.com/dmz006/imap-mcp/internal/inbound"
	"github.com/dmz006/imap-mcp/internal/intel"
	mcpserver "github.com/dmz006/imap-mcp/internal/mcp"
	"github.com/dmz006/imap-mcp/internal/mcp/tools"
	"github.com/dmz006/imap-mcp/internal/output"
	"github.com/dmz006/imap-mcp/internal/server"
	"github.com/dmz006/imap-mcp/internal/sync"
	"github.com/dmz006/imap-mcp/internal/trust"
	"github.com/dmz006/imap-mcp/internal/webhook"
	mcpgo "github.com/mark3labs/mcp-go/server"
)

var Version = config.Version

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return runStdio()
	}

	switch os.Args[1] {
	case "serve":
		return runServe(os.Args[2:])
	case "auth-setup":
		return runAuthSetup(os.Args[2:])
	case "run-rules":
		return runRules(os.Args[2:])
	case "db":
		return runDB(os.Args[2:])
	case "version":
		fmt.Println(Version)
		return nil
	case "help", "--help", "-h":
		printUsage()
		return nil
	default:
		// Unknown subcommand — fall through to stdio (allows `imap-mcp` prefix from claude)
		return runStdio()
	}
}

// runStdio starts the MCP server over stdin/stdout for Claude Code integration.
func runStdio() error {
	cfg, log, err := loadConfig("")
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	deps, cleanup, err := buildDeps(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer cleanup()

	mcpSrv := mcpserver.NewServer(cfg, deps.pool, deps.db, deps.syncer, deps.out, deps.pipeline, false)
	stdio := mcpgo.NewStdioServer(mcpSrv)

	log.Info("imap-mcp running in stdio mode", "version", Version)
	return stdio.Listen(ctx, os.Stdin, os.Stdout)
}

// runRules applies all active automation rules once and exits. Designed to be
// invoked by a scheduler (e.g. an hourly datawatch cron) for unattended inbox
// triage + categorization. Connects, runs rules, prints a summary.
func runRules(args []string) error {
	fs := flag.NewFlagSet("run-rules", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file path")
	dryRun := fs.Bool("dry-run", false, "preview match counts without acting")
	_ = fs.Parse(args)

	cfg, log, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// Rules live in the state DB. The cache is opened too when it can be:
	// new_sender rules read its classification halls (D30). Without it they
	// treat every message as not classified yet.
	var database *db.DB
	if keys, kerr := cfg.ResolveDBKeys(true, true); kerr == nil {
		database, err = db.Open(db.Options{Path: cfg.DB.Path, Key: keys.State},
			db.Options{Path: cfg.DB.Cache.Path, Key: keys.Cache})
		if err != nil {
			log.Warn("run-rules: cache unavailable, continuing with rules only", "err", err)
			database = nil
		}
	}
	if database == nil {
		keys, err := cfg.ResolveDBKeys(true, false)
		if err != nil {
			return err
		}
		if database, err = db.OpenState(db.Options{Path: cfg.DB.Path, Key: keys.State}); err != nil {
			return fmt.Errorf("open db: %w", err)
		}
	}
	defer database.Close()
	logMigration(log)

	// rule.fired and hold.digest go into the webhook outbox; serve delivers
	// them. Only those: connection events from this short-lived process are noise.
	b := bus.New()
	enq := webhook.NewEnqueuer(database.Webhooks, log, nil)
	b.Subscribe(bus.EventRuleFired, enq.Handle)
	b.Subscribe(bus.EventHoldDigest, enq.Handle)
	pool := imap.NewPool(cfg, b, log)
	if err := pool.Connect(ctx); err != nil {
		return fmt.Errorf("connect accounts: %w", err)
	}
	defer pool.Close()

	h := tools.NewHandlers(cfg, pool, database, nil, nil)
	results, err := h.RunActiveRules(0, *dryRun)
	if err != nil {
		return fmt.Errorf("run rules: %w", err)
	}

	total := 0
	for _, r := range results {
		total += r.Matched
		line := fmt.Sprintf("  %-22s %-6s x%d", r.Name, r.Action, r.Matched)
		if r.Error != "" {
			line += "  ERROR: " + r.Error
		}
		fmt.Println(line)
	}
	fmt.Printf("run-rules: %d active rules, %d messages actioned (dry_run=%v)\n", len(results), total, *dryRun)
	return nil
}

// runDB handles database maintenance subcommands.
func runDB(args []string) error {
	if len(args) == 0 || args[0] != "encrypt" {
		return fmt.Errorf("usage: imap-mcp db encrypt [--only state|cache] [--config PATH]")
	}
	fs := flag.NewFlagSet("db encrypt", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file path")
	only := fs.String("only", "", "encrypt only this file: state or cache")
	_ = fs.Parse(args[1:])
	if *only != "" && *only != "state" && *only != "cache" {
		return fmt.Errorf("--only must be state or cache")
	}
	cfg, _, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	doState, doCache := *only != "cache", *only != "state"
	keys, err := cfg.ResolveDBKeys(doState, doCache)
	if err != nil {
		return err
	}
	return encryptFiles(os.Stdout, []encryptTarget{
		{"state", "db.encryption_key", cfg.DB.Path, keys.State, doState},
		{"cache", "db.cache.encryption_key", cfg.DB.Cache.Path, keys.Cache, doCache},
	})
}

type encryptTarget struct {
	name, keyField, path, key string
	enabled                   bool
}

// encryptFiles converts each enabled target (AGENT.md D14) and lists any
// plaintext copies left for the operator to remove.
func encryptFiles(w io.Writer, targets []encryptTarget) error {
	var failed []string
	for _, t := range targets {
		if !t.enabled {
			continue
		}
		if t.key == "" {
			fmt.Fprintf(w, "%s: %s not set; leaving %s as is\n", t.name, t.keyField, t.path)
			continue
		}
		rep, err := db.Encrypt(t.path, t.key)
		switch {
		case errors.Is(err, db.ErrAlreadyEncrypted):
			fmt.Fprintf(w, "%s: %s is already encrypted\n", t.name, t.path)
			continue
		case errors.Is(err, db.ErrNoDatabase):
			fmt.Fprintf(w, "%s: %s does not exist yet; it will be created encrypted\n", t.name, t.path)
			continue
		case err != nil:
			fmt.Fprintf(w, "%s: FAILED: %v\n", t.name, err)
			failed = append(failed, t.name)
			continue
		}
		fmt.Fprintf(w, "%s: encrypted %s (%d tables, %d rows verified); plaintext original removed\n",
			t.name, rep.Path, rep.Tables, rep.Rows)
		if len(rep.PlaintextCopies) > 0 {
			fmt.Fprintf(w, "%s: these plaintext copies still exist; delete them once you no longer need them:\n", t.name)
			for _, p := range rep.PlaintextCopies {
				fmt.Fprintf(w, "  %s\n", p)
			}
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("encrypt failed for: %s", strings.Join(failed, ", "))
	}
	return nil
}

// runServe starts the combined HTTP server (MCP + REST API).
func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "Path to config.yaml")
	_ = fs.Parse(args)

	cfg, log, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}

	// Resolve HTTP auth before connecting anything: it fails closed (D13a-2).
	authn, err := buildAuth(cfg, log)
	if err != nil {
		return err
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	deps, cleanup, err := buildDeps(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer cleanup()

	srv := server.New(cfg, deps.pool, deps.db, deps.bus, deps.syncer, deps.pipeline, deps.out, log, authn)

	// Webhooks (D16): enqueue every event into the durable outbox and deliver
	// from it, including rows queued by the run-rules CLI.
	dispatcher := webhook.NewDispatcher(deps.db.Webhooks, deps.bus, log)
	enqueuer := webhook.NewEnqueuer(deps.db.Webhooks, log, dispatcher.Wake)
	enqueuer.Attach(deps.bus)
	srv.SetWebhooks(enqueuer)
	go dispatcher.Run(ctx)

	return srv.Start(ctx)
}

// logMigration reports a legacy single-file split performed by db.OpenState.
func logMigration(log *slog.Logger) {
	if m := db.LastMigration; m != nil {
		log.Info("migrated legacy imap.db: cache tables moved to cache.db (rebuilt from IMAP); state verified unchanged",
			"backup", m.BackupPath, "rules", m.Counts["rules"], "webhooks", m.Counts["webhooks"], "nonces", m.Counts["inbound_nonces"])
	}
}

// buildAuth resolves server.auth into an Authenticator. It returns nil only
// when auth is explicitly disabled, which is logged as a warning.
func buildAuth(cfg *config.Config, log *slog.Logger) (*httpauth.Authenticator, error) {
	tokens, disabled, err := cfg.ServeAuth()
	if err != nil {
		return nil, fmt.Errorf("server auth: %w", err)
	}
	if disabled {
		log.Warn("server.auth.disabled is set: /api and /mcp accept unauthenticated requests from any local process (insecure)")
		return nil, nil
	}
	authn, err := httpauth.New(tokens, []string{"/api/health"}, log)
	if err != nil {
		return nil, fmt.Errorf("server auth: %w", err)
	}
	names := make([]string, len(tokens))
	for i, t := range tokens {
		names[i] = t.Name
	}
	log.Info("server auth enabled", "tokens", names)
	return authn, nil
}

// runAuthSetup runs the OAuth2 browser flow for an account.
func runAuthSetup(args []string) error {
	fs := flag.NewFlagSet("auth-setup", flag.ExitOnError)
	account := fs.String("account", "", "Account name from config")
	cfgPath := fs.String("config", defaultConfigPath(), "Path to config.yaml")
	_ = fs.Parse(args)

	cfg, _, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}

	name := *account
	if name == "" {
		name = cfg.DefaultAccount().Name
	}
	a, err := cfg.Account(name)
	if err != nil {
		return err
	}
	if a.Auth.Type != "xoauth2" {
		return fmt.Errorf("account %q uses %s auth, not xoauth2 (service-account auth needs no setup step)", name, a.Auth.Type)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	return auth.RunAuthSetup(ctx,
		a.Auth.Username,
		a.Auth.ClientID,
		a.Auth.ClientSecret,
		a.Auth.TokenFile,
		a.Auth.Provider,
	)
}

// deps holds all wired-up application components.
type deps struct {
	pool     *imap.Pool
	db       *db.DB
	bus      *bus.Bus
	syncer   *sync.Syncer
	pipeline *enrichment.Pipeline
	out      *output.Writer
}

func buildDeps(ctx context.Context, cfg *config.Config, log *slog.Logger) (*deps, func(), error) {
	b := bus.New()

	keys, err := cfg.ResolveDBKeys(true, true)
	if err != nil {
		return nil, nil, err
	}
	database, err := db.Open(
		db.Options{Path: cfg.DB.Path, Key: keys.State},
		db.Options{Path: cfg.DB.Cache.Path, Key: keys.Cache},
	)
	if err != nil {
		return nil, nil, fmt.Errorf("open db: %w", err)
	}
	logMigration(log)
	log.Info("storage", "state_encrypted", keys.State != "", "cache_encrypted", keys.Cache != "")

	pool := imap.NewPool(cfg, b, log)
	if err := pool.Connect(ctx); err != nil {
		database.Close()
		return nil, nil, fmt.Errorf("connect accounts: %w", err)
	}

	out, err := output.New(cfg.WorkingDir)
	if err != nil {
		pool.Close()
		database.Close()
		return nil, nil, fmt.Errorf("output dir: %w", err)
	}
	log.Info("working directory", "path", out.Root())

	syncer := sync.New(cfg, pool, database, b, log)
	var popts []enrichment.Option
	if cfg.Datawatch != nil {
		dwTransport, err := cfg.Datawatch.Transport() // validated at config load
		if err != nil {
			pool.Close()
			database.Close()
			return nil, nil, err
		}
		popts = append(popts, enrichment.WithDatawatch(cfg.Datawatch.APIURL, cfg.Datawatch.Token, dwTransport))
	}
	pipeline := enrichment.NewPipeline(cfg.Enrichment, database, b, log, popts...)

	// Inbound command channel (trust-gated). The watcher is a no-op unless an
	// account enables inbound, so it is always safe to start.
	verifier := trust.NewVerifier(database.Nonces)
	watcher := inbound.NewWatcher(cfg, pool, inbound.NewProcessor(b, verifier), log)

	// Sender intelligence (D19, D20, D28): header scan + roles. Model-assigned
	// roles go through the enrichment gates and only when enrichment is on.
	var classify intel.ClassifyFunc
	if cfg.Enrichment.Enabled {
		classify = func(ctx context.Context, prompt string) (string, error) {
			out, err := pipeline.ClassifyWhenIdle(ctx, prompt)
			if errors.Is(err, enrichment.ErrIdleGated) {
				return "", intel.ErrGated
			}
			return out, err
		}
	}
	scanner := intel.New(cfg, intel.NewIMAPSource(pool), database.StateSQL(), database.SQL(), classify, log)
	scanner.SetBus(b) // anomaly.detected (D22)

	// Start background workers
	go syncer.Run(ctx)
	go pipeline.Run(ctx)
	go scanner.Run(ctx)
	go watcher.Run(ctx)
	go pool.StartKeepalive(ctx, 4*time.Minute) // keep IMAP connections warm + self-heal

	cleanup := func() {
		pool.Close()
		database.Close()
	}

	return &deps{
		pool:     pool,
		db:       database,
		bus:      b,
		syncer:   syncer,
		pipeline: pipeline,
		out:      out,
	}, cleanup, nil
}

func loadConfig(path string) (*config.Config, *slog.Logger, error) {
	if path == "" {
		path = defaultConfigPath()
	}

	// Determine log level before config loads (needed for startup errors)
	logLevel := slog.LevelInfo
	if os.Getenv("IMAP_MCP_LOG_LEVEL") == "debug" {
		logLevel = slog.LevelDebug
	}
	_ = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel}))

	cfg, err := config.Load(path)
	if err != nil {
		return nil, nil, fmt.Errorf("load config %s: %w", path, err)
	}

	// Re-initialize logger with configured level
	switch cfg.Log.Level {
	case "debug":
		logLevel = slog.LevelDebug
	case "warn":
		logLevel = slog.LevelWarn
	case "error":
		logLevel = slog.LevelError
	}
	var handler slog.Handler
	if cfg.Log.Format == "json" {
		handler = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	} else {
		handler = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevel})
	}
	return cfg, slog.New(handler), nil
}

func defaultConfigPath() string {
	home, _ := os.UserHomeDir()
	paths := []string{
		"config.yaml",
		home + "/.config/imap-mcp/config.yaml",
	}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return paths[1]
}

func printUsage() {
	fmt.Printf(`imap-mcp %s — IMAP server with MCP + REST API

Usage:
  imap-mcp                        Run as MCP stdio server (for Claude Code)
  imap-mcp serve [--config PATH]  Run as HTTP server (MCP at /mcp, REST at /api)
  imap-mcp auth-setup [--account NAME] [--config PATH]
                                  Run OAuth2 browser flow for an account
  imap-mcp run-rules [--dry-run] [--config PATH]
                                  Apply active rules once and exit
  imap-mcp db encrypt [--only state|cache] [--config PATH]
                                  Encrypt existing plaintext DBs in place with
                                  the configured keys (stop the service first)
  imap-mcp version                Print version

Config: ~/.config/imap-mcp/config.yaml (or --config flag)
Docs:   see IMAP-MCP-CONTEXT.md in the project root
`, Version)
}
