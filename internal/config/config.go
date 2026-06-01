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

var Version = "0.1.0"

type Config struct {
	Accounts   []AccountConfig   `yaml:"accounts"`
	Server     ServerConfig      `yaml:"server"`
	DB         DBConfig          `yaml:"db"`
	WorkingDir string            `yaml:"working_dir"`
	Enrichment EnrichmentConfig  `yaml:"enrichment"`
	Sync       SyncConfig        `yaml:"sync"`
	Log        LogConfig         `yaml:"log"`
}

type AccountConfig struct {
	Name    string     `yaml:"name"`
	Default bool       `yaml:"default"`
	IMAP    IMAPConfig `yaml:"imap"`
	Auth    AuthConfig `yaml:"auth"`
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

func (c *Config) Account(name string) (*AccountConfig, error) {
	for i := range c.Accounts {
		if c.Accounts[i].Name == name {
			return &c.Accounts[i], nil
		}
	}
	return nil, fmt.Errorf("account %q not found", name)
}
