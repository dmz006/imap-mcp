// Package config loads and validates imap-mcp configuration from YAML files
// and environment variable overrides.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var Version = "0.2.0"

type Config struct {
	Accounts   []AccountConfig  `yaml:"accounts"`
	Server     ServerConfig     `yaml:"server"`
	DB         DBConfig         `yaml:"db"`
	WorkingDir string           `yaml:"working_dir"`
	Enrichment EnrichmentConfig `yaml:"enrichment"`
	Sync       SyncConfig       `yaml:"sync"`
	Log        LogConfig        `yaml:"log"`
	// Datawatch is optional. When present, ${secret:name} references in
	// credentials resolve against a datawatch secrets service. When absent,
	// imap-mcp runs fully standalone (plain values / ${ENV_VAR} only).
	Datawatch *DatawatchConfig `yaml:"datawatch,omitempty"`
}

type AccountConfig struct {
	Name    string     `yaml:"name"`
	Default bool       `yaml:"default"`
	IMAP    IMAPConfig `yaml:"imap"`
	Auth    AuthConfig `yaml:"auth"`
	// SMTP is optional outbound config for this domain. When absent, the
	// account is receive-only. Credentials default to the Auth block.
	SMTP *SMTPConfig `yaml:"smtp,omitempty"`
	// Inbound optionally turns this account into a trust-gated command
	// channel: new mail in the watched folder is evaluated against the
	// configured gates and, only if all required gates pass, emitted as a
	// verified command event. Absent = no command channel (read/manage only).
	Inbound *InboundConfig `yaml:"inbound,omitempty"`
}

// SMTPConfig is per-account outbound mail. Each receiving domain sends through
// its own server rather than a shared relay.
type SMTPConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`     // default 587 (starttls) or 465 (tls)
	TLS      bool   `yaml:"tls"`      // implicit TLS (port 465)
	StartTLS bool   `yaml:"starttls"` // STARTTLS upgrade (port 587)
	Username string `yaml:"username"` // default: Auth.Username
	Password string `yaml:"password"` // default: Auth.Password; supports ${secret:}/${ENV}
	From     string `yaml:"from"`     // default: Auth.Username
}

// InboundConfig configures the trust-gated inbound command channel.
type InboundConfig struct {
	Enabled      bool       `yaml:"enabled"`
	WatchFolder  string     `yaml:"watch_folder"` // default INBOX
	Gates        GateConfig `yaml:"gates"`
	Capabilities []string   `yaml:"capabilities"` // allowed command verbs (default-deny)
}

// GateConfig declares which composable trust checks an inbound message must
// pass before it may trigger an action. All configured gates are required
// (AND). With no gates configured, nothing can trigger (default-deny).
type GateConfig struct {
	// Allowlist of sender addresses/domains. Required when non-empty.
	Allowlist []string `yaml:"allowlist,omitempty"`
	// RequireDKIM/RequireDMARC enforce a pass verdict in the receiving
	// server's Authentication-Results header.
	RequireDKIM  bool `yaml:"require_dkim,omitempty"`
	RequireDMARC bool `yaml:"require_dmarc,omitempty"`
	// HMACSecret, when set, requires a valid HMAC over the command envelope.
	// Supports ${secret:}/${ENV} references.
	HMACSecret string `yaml:"hmac_secret,omitempty"`
	// ReplayWindowMinutes bounds command freshness; 0 disables the window
	// check (nonce uniqueness is always enforced when a command is present).
	ReplayWindowMinutes int `yaml:"replay_window_minutes,omitempty"`
	// RequirePGP enables the PGP-signed-envelope gate. BACKLOG: until the
	// crypto is implemented this gate fails closed (rejects everything).
	RequirePGP bool `yaml:"require_pgp,omitempty"`
}

type IMAPConfig struct {
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	TLS      bool   `yaml:"tls"`
	StartTLS bool   `yaml:"starttls"`
}

type AuthConfig struct {
	Type         string `yaml:"type"` // "plain" | "xoauth2"
	Username     string `yaml:"username"`
	Password     string `yaml:"password"`
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
	TokenFile    string `yaml:"token_file"`
}

type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
}

type DBConfig struct {
	Path string `yaml:"path"`
}

type EnrichmentConfig struct {
	Enabled    bool   `yaml:"enabled"`
	OllamaURL  string `yaml:"ollama_url"`
	EmbedModel string `yaml:"embed_model"`
	LLMModel   string `yaml:"llm_model"`
	BatchSize  int    `yaml:"batch_size"`
	AutoSync   bool   `yaml:"auto_sync"`
}

type SyncConfig struct {
	IntervalMinutes  int      `yaml:"interval_minutes"`
	FullSyncOnStart  bool     `yaml:"full_sync_on_start"`
	Folders          []string `yaml:"folders"`
}

type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	// Expand environment variable references before parsing
	expanded := expandEnvRefs(string(data))

	cfg := defaults()
	if err := yaml.Unmarshal([]byte(expanded), cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	applyEnvOverrides(cfg)
	expandPaths(cfg)

	// Resolve ${secret:name} references against datawatch, if configured.
	// No-op (and no datawatch dependency) when no such references are used.
	if err := resolveSecrets(cfg); err != nil {
		return nil, fmt.Errorf("resolve secrets: %w", err)
	}

	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	return cfg, nil
}

func defaults() *Config {
	return &Config{
		WorkingDir: "~/workspace/email",
		Server: ServerConfig{
			Host: "127.0.0.1",
			Port: 8765,
		},
		DB: DBConfig{
			Path: "~/.local/share/imap-mcp/imap.db",
		},
		Enrichment: EnrichmentConfig{
			Enabled:    true,
			OllamaURL:  "http://localhost:11434",
			EmbedModel: "nomic-embed-text",
			LLMModel:   "qwen3:1.7b",
			BatchSize:  20,
			AutoSync:   true,
		},
		Sync: SyncConfig{
			IntervalMinutes: 15,
			FullSyncOnStart: true,
			Folders:         []string{"INBOX"},
		},
		Log: LogConfig{
			Level:  "info",
			Format: "text",
		},
	}
}

var envRefRe = regexp.MustCompile(`\$\{([^}]+)\}`)

func expandEnvRefs(s string) string {
	return envRefRe.ReplaceAllStringFunc(s, func(m string) string {
		key := envRefRe.FindStringSubmatch(m)[1]
		// Leave ${secret:name} references intact — they are resolved later
		// against datawatch, after the config (incl. the datawatch block) parses.
		if strings.HasPrefix(key, "secret:") {
			return m
		}
		if val := os.Getenv(key); val != "" {
			return val
		}
		return m
	})
}

// applyEnvOverrides applies IMAP_MCP_* environment variables on top of YAML values.
// Convention: IMAP_MCP_SERVER_PORT → cfg.Server.Port
func applyEnvOverrides(cfg *Config) {
	if v := os.Getenv("IMAP_MCP_SERVER_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.Server.Port = p
		}
	}
	if v := os.Getenv("IMAP_MCP_SERVER_HOST"); v != "" {
		cfg.Server.Host = v
	}
	if v := os.Getenv("IMAP_MCP_DB_PATH"); v != "" {
		cfg.DB.Path = v
	}
	if v := os.Getenv("IMAP_MCP_OLLAMA_URL"); v != "" {
		cfg.Enrichment.OllamaURL = v
	}
	if v := os.Getenv("IMAP_MCP_LOG_LEVEL"); v != "" {
		cfg.Log.Level = v
	}
}

func expandPaths(cfg *Config) {
	home, _ := os.UserHomeDir()
	expand := func(p string) string {
		if strings.HasPrefix(p, "~/") {
			return filepath.Join(home, p[2:])
		}
		return p
	}
	cfg.DB.Path = expand(cfg.DB.Path)
	cfg.WorkingDir = expand(cfg.WorkingDir)
	for i := range cfg.Accounts {
		cfg.Accounts[i].Auth.TokenFile = expand(cfg.Accounts[i].Auth.TokenFile)
	}
}

func validate(cfg *Config) error {
	if len(cfg.Accounts) == 0 {
		return fmt.Errorf("at least one account is required")
	}
	defaults := 0
	for _, a := range cfg.Accounts {
		if a.Name == "" {
			return fmt.Errorf("account missing name")
		}
		if a.IMAP.Host == "" {
			return fmt.Errorf("account %q: imap.host is required", a.Name)
		}
		if a.Auth.Type == "" {
			return fmt.Errorf("account %q: auth.type is required", a.Name)
		}
		if a.Default {
			defaults++
		}
		if a.SMTP != nil && a.SMTP.Host == "" {
			return fmt.Errorf("account %q: smtp.host is required when smtp is configured", a.Name)
		}
		if a.Inbound != nil && a.Inbound.Enabled {
			g := a.Inbound.Gates
			if len(g.Allowlist) == 0 && !g.RequireDKIM && !g.RequireDMARC && g.HMACSecret == "" && !g.RequirePGP {
				return fmt.Errorf("account %q: inbound is enabled but no trust gates are configured; "+
					"refusing to expose an ungated command channel (default-deny)", a.Name)
			}
		}
	}
	if defaults == 0 {
		cfg.Accounts[0].Default = true
	}
	if defaults > 1 {
		return fmt.Errorf("only one account may be marked as default")
	}
	return nil
}

func (c *Config) DefaultAccount() *AccountConfig {
	for i := range c.Accounts {
		if c.Accounts[i].Default {
			return &c.Accounts[i]
		}
	}
	return &c.Accounts[0]
}

// ResolvedSMTP returns the SMTP config with credentials and From defaulted
// from the account's Auth block, and a default port chosen from TLS/StartTLS.
// Returns nil if the account has no SMTP config.
func (a *AccountConfig) ResolvedSMTP() *SMTPConfig {
	if a.SMTP == nil {
		return nil
	}
	s := *a.SMTP // copy — never mutate the loaded config
	if s.Username == "" {
		s.Username = a.Auth.Username
	}
	if s.Password == "" {
		s.Password = a.Auth.Password
	}
	if s.From == "" {
		s.From = a.Auth.Username
	}
	if s.Port == 0 {
		if s.TLS {
			s.Port = 465
		} else {
			s.Port = 587
		}
	}
	return &s
}

func (c *Config) Account(name string) (*AccountConfig, error) {
	for i := range c.Accounts {
		if c.Accounts[i].Name == name {
			return &c.Accounts[i], nil
		}
	}
	return nil, fmt.Errorf("account %q not found", name)
}
