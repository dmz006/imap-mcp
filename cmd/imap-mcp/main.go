package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/enrichment"
	"github.com/dmz006/imap-mcp/internal/imap"
	"github.com/dmz006/imap-mcp/internal/imap/auth"
	"github.com/dmz006/imap-mcp/internal/inbound"
	mcpserver "github.com/dmz006/imap-mcp/internal/mcp"
	"github.com/dmz006/imap-mcp/internal/output"
	"github.com/dmz006/imap-mcp/internal/server"
	"github.com/dmz006/imap-mcp/internal/sync"
	"github.com/dmz006/imap-mcp/internal/trust"
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

	mcpSrv := mcpserver.NewServer(cfg, deps.pool, deps.db, deps.syncer, deps.out)
	stdio := mcpgo.NewStdioServer(mcpSrv)

	log.Info("imap-mcp running in stdio mode", "version", Version)
	return stdio.Listen(ctx, os.Stdin, os.Stdout)
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

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	deps, cleanup, err := buildDeps(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer cleanup()

	srv := server.New(cfg, deps.pool, deps.db, deps.bus, deps.syncer, deps.pipeline, deps.out, log)
	return srv.Start(ctx)
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
		return fmt.Errorf("account %q uses %s auth, not xoauth2", name, a.Auth.Type)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	return auth.RunAuthSetup(ctx,
		a.Auth.Username,
		a.Auth.ClientID,
		a.Auth.ClientSecret,
		a.Auth.TokenFile,
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

	database, err := db.Open(cfg.DB.Path)
	if err != nil {
		return nil, nil, fmt.Errorf("open db: %w", err)
	}

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
	pipeline := enrichment.NewPipeline(cfg.Enrichment, database, b, log)

	// Inbound command channel (trust-gated). The watcher is a no-op unless an
	// account enables inbound, so it is always safe to start.
	verifier := trust.NewVerifier(database.Nonces)
	watcher := inbound.NewWatcher(cfg, pool, inbound.NewProcessor(b, verifier), log)

	// Start background workers
	go syncer.Run(ctx)
	go pipeline.Run(ctx)
	go watcher.Run(ctx)

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
  imap-mcp version                Print version

Config: ~/.config/imap-mcp/config.yaml (or --config flag)
Docs:   see IMAP-MCP-CONTEXT.md in the project root
`, Version)
}
