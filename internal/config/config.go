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
	"time"

	"gopkg.in/yaml.v3"
)

var Version = "0.16.0"

type Config struct {
	Accounts   []AccountConfig  `yaml:"accounts"`
	Server     ServerConfig     `yaml:"server"`
	DB         DBConfig         `yaml:"db"`
	WorkingDir string           `yaml:"working_dir"`
	Enrichment EnrichmentConfig `yaml:"enrichment"`
	Sync       SyncConfig       `yaml:"sync"`
	Tools      ToolsConfig      `yaml:"tools"`
	Intel      IntelConfig      `yaml:"intelligence"`
	Rules      RulesConfig      `yaml:"rules"`
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
	// Sync optionally overrides the global sync settings for this account.
	Sync *AccountSyncConfig `yaml:"sync,omitempty"`
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
	Type         string `yaml:"type"` // "plain" | "xoauth2" | "xoauth2_service_account"
	Username     string `yaml:"username"`
	Password     string `yaml:"password"`
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
	TokenFile    string `yaml:"token_file"`
	// Provider selects the OAuth2 endpoint for xoauth2: "google" | "microsoft".
	// REQUIRED for enterprise Gmail / Google Workspace (custom domains), since
	// the address domain can't be auto-detected. Empty falls back to domain
	// heuristics (gmail.com → google, else microsoft).
	Provider string `yaml:"provider,omitempty"`
	// ServiceAccountFile is the path to a GCP service-account JSON key, used by
	// the "xoauth2_service_account" type (domain-wide delegation — headless,
	// admin-authorized, no browser/per-user token).
	ServiceAccountFile string `yaml:"service_account_file,omitempty"`
	// Subject is the Workspace user to impersonate for service-account auth.
	// Defaults to Username.
	Subject string `yaml:"subject,omitempty"`
}

type ServerConfig struct {
	Host string           `yaml:"host"`
	Port int              `yaml:"port"`
	Auth ServerAuthConfig `yaml:"auth"`
}

// ServerAuthConfig configures bearer-token auth for `serve` (AGENT.md D13a, D13a-2).
// At least one token is required unless Disabled is explicitly set.
type ServerAuthConfig struct {
	// Disabled turns token auth off (insecure). Logged at every startup and
	// reported in /api/health.
	Disabled bool `yaml:"disabled"`
	// Tokens are the accepted bearer tokens.
	Tokens []TokenConfig `yaml:"tokens"`
}

// TokenConfig is one named bearer token. Token must be a ${secret:name} or
// ${ENV} reference in YAML; Scopes is any of read, write, send, admin.
type TokenConfig struct {
	Name   string   `yaml:"name"`
	Token  string   `yaml:"token"`
	Scopes []string `yaml:"scopes"`
}

// DBConfig locates the two SQLite files (AGENT.md D1b). Path is the state DB
// (rules, webhooks, nonces); Cache is the disposable mail cache. Each file is
// optionally encrypted with its own key (D1, D5).
type DBConfig struct {
	Path          string        `yaml:"path"`
	EncryptionKey string        `yaml:"encryption_key"`
	Cache         CacheDBConfig `yaml:"cache"`
}

// CacheDBConfig locates the cache DB. An empty Path means cache.db next to
// the state DB.
type CacheDBConfig struct {
	Path          string `yaml:"path"`
	EncryptionKey string `yaml:"encryption_key"`
}

// EnrichmentConfig controls background enrichment (AGENT.md D11a, D11b).
// OllamaURL/EmbedModel/LLMModel are the defaults for the per-call-type
// provider settings below.
type EnrichmentConfig struct {
	Enabled    bool   `yaml:"enabled"`
	OllamaURL  string `yaml:"ollama_url"`
	EmbedModel string `yaml:"embed_model"`
	LLMModel   string `yaml:"llm_model"`
	BatchSize  int    `yaml:"batch_size"`
	AutoSync   bool   `yaml:"auto_sync"`

	// Embed always calls Ollama directly (the datawatch proxy has no embeddings).
	Embed EmbedProviderConfig `yaml:"embed"`
	// Classify picks the classification provider.
	Classify ClassifyProviderConfig `yaml:"classify"`

	// Concurrency caps in-flight calls per provider (default 2).
	Concurrency int `yaml:"concurrency"`
	// BackfillPerMinute rate-limits the backfill lane (default 30; 0 = unlimited).
	// New mail is never rate-limited.
	BackfillPerMinute int `yaml:"backfill_per_minute"`
	// MaxAttempts before a message is marked error (default 3).
	MaxAttempts int `yaml:"max_attempts"`
	// BackoffMaxSeconds caps the exponential backoff after provider errors (default 300).
	BackoffMaxSeconds int `yaml:"backoff_max_seconds"`
	// BackfillWindow restricts backfill to local hours "HH:MM-HH:MM" (may wrap
	// midnight). Empty = no restriction. New mail is never restricted.
	BackfillWindow string `yaml:"backfill_window"`
	// Yield pauses backfill while datawatch or Ollama is busy.
	Yield YieldConfig `yaml:"yield"`
}

// EmbedProviderConfig is the embedding provider (direct Ollama).
type EmbedProviderConfig struct {
	URL   string `yaml:"url"`   // default: enrichment.ollama_url
	Model string `yaml:"model"` // default: enrichment.embed_model
}

// ClassifyProviderConfig is the classification provider.
type ClassifyProviderConfig struct {
	// Provider is "ollama" (direct, default) or "datawatch" (POST
	// /api/proxy/llm/<datawatch_llm> using the datawatch block's api_url/token).
	Provider     string `yaml:"provider"`
	URL          string `yaml:"url"`           // ollama: default enrichment.ollama_url
	Model        string `yaml:"model"`         // ollama: default enrichment.llm_model
	DatawatchLLM string `yaml:"datawatch_llm"` // datawatch: LLM registry name
}

// YieldConfig makes backfill yield to other GPU work (D11b). It never pauses
// new-mail enrichment.
type YieldConfig struct {
	// Enabled turns yielding on (default true). The datawatch capacity check
	// needs a datawatch block; the Ollama check always applies.
	Enabled bool `yaml:"enabled"`
	// MaxForeignResidentGB pauses backfill while models other than ours occupy
	// more than this much memory on the target Ollama (default 8).
	MaxForeignResidentGB float64 `yaml:"max_foreign_resident_gb"`
	// DatawatchPools are datawatch capacity pools to respect, e.g.
	// "node:<compute-node>" for the node running our Ollama. With the
	// datawatch classify provider, "llm:<datawatch_llm>" is added automatically.
	DatawatchPools []string `yaml:"datawatch_pools"`
}

// EmbedURL, EmbedModelName, ClassifyURL, ClassifyModel resolve provider
// settings against the legacy top-level defaults.
func (e EnrichmentConfig) EmbedURL() string       { return firstNonEmpty(e.Embed.URL, e.OllamaURL) }
func (e EnrichmentConfig) EmbedModelName() string { return firstNonEmpty(e.Embed.Model, e.EmbedModel) }
func (e EnrichmentConfig) ClassifyURL() string    { return firstNonEmpty(e.Classify.URL, e.OllamaURL) }
func (e EnrichmentConfig) ClassifyModel() string  { return firstNonEmpty(e.Classify.Model, e.LLMModel) }
func (e EnrichmentConfig) ClassifyProvider() string {
	return firstNonEmpty(e.Classify.Provider, "ollama")
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// SyncConfig controls the background mail cache sync (AGENT.md D8, D10).
type SyncConfig struct {
	IntervalMinutes int  `yaml:"interval_minutes"`
	FullSyncOnStart bool `yaml:"full_sync_on_start"`
	// Folders to cache: SPECIAL-USE tokens (\Sent, \Archive, \Drafts,
	// \Junk, \All, \Flagged) or literal names. Default INBOX + \Sent.
	Folders []string `yaml:"folders"`
	// WindowDays is the rolling cache window by IMAP INTERNALDATE (default 30).
	WindowDays int `yaml:"window_days"`
	// FolderWindowDays overrides WindowDays per folder entry (token or name).
	FolderWindowDays map[string]int `yaml:"folder_window_days"`
	// MaxMessageMB caps the size of a message whose full body is fetched;
	// larger messages are cached headers-only (default 25).
	MaxMessageMB int `yaml:"max_message_mb"`
	// KeepFlagged keeps \Flagged messages cached (and fetches them) even
	// outside the window (D7). Default off.
	KeepFlagged bool `yaml:"keep_flagged"`
	// VacuumIntervalHours is the minimum time between automatic VACUUMs of the
	// cache file; the check runs after every sync cycle's cleaning pass
	// (default 24; 0 disables automatic VACUUM).
	VacuumIntervalHours int `yaml:"vacuum_interval_hours"`
}

// AccountSyncConfig overrides SyncConfig for one account. Folders replaces
// the global list; zero values inherit.
type AccountSyncConfig struct {
	Folders          []string       `yaml:"folders"`
	WindowDays       int            `yaml:"window_days"`
	FolderWindowDays map[string]int `yaml:"folder_window_days"`
}

// SyncFolders returns the folder entries to cache for an account.
func (c *Config) SyncFolders(a *AccountConfig) []string {
	if a != nil && a.Sync != nil && len(a.Sync.Folders) > 0 {
		return a.Sync.Folders
	}
	if len(c.Sync.Folders) > 0 {
		return c.Sync.Folders
	}
	return []string{"INBOX", `\Sent`}
}

// WindowDays resolves the cache window for one folder entry of an account:
// account folder override > global folder override > account > global > 30.
func (c *Config) WindowDays(a *AccountConfig, folder string) int {
	if a != nil && a.Sync != nil {
		if d := a.Sync.FolderWindowDays[folder]; d > 0 {
			return d
		}
	}
	if d := c.Sync.FolderWindowDays[folder]; d > 0 {
		return d
	}
	if a != nil && a.Sync != nil && a.Sync.WindowDays > 0 {
		return a.Sync.WindowDays
	}
	if c.Sync.WindowDays > 0 {
		return c.Sync.WindowDays
	}
	return 30
}

// ToolsConfig limits the content tools (AGENT.md D24, D25): attachment
// fetches and message exports, which write into working_dir.
type ToolsConfig struct {
	// AttachmentInlineKB: text/* attachments up to this size are also returned
	// inline (decoded) by get_attachments. 0 = never inline. Default 64.
	AttachmentInlineKB int `yaml:"attachment_inline_kb"`
	// AttachmentMaxMB caps the size of one fetched attachment. Default 25.
	AttachmentMaxMB int `yaml:"attachment_max_mb"`
	// ExportMaxMessages caps the messages in one export_message batch. Default 500.
	ExportMaxMessages int `yaml:"export_max_messages"`
	// ExportMaxMB caps the total size of one export. Default 100.
	ExportMaxMB int `yaml:"export_max_mb"`
}

// RulesConfig tunes rule runs. The held-mail digest (AGENT.md D31) lists what
// new_sender rules held since the last digest.
type RulesConfig struct {
	// HoldDigest turns the daily digest on (default true). Pointer so an
	// explicit false is kept.
	HoldDigest *bool `yaml:"hold_digest"`
	// HoldDigestHour: the first full rule run at or after this local hour
	// (0-23) sends the day's digest. Default 8.
	HoldDigestHour int `yaml:"hold_digest_hour"`
}

// HoldDigestOn reports whether the held-mail digest is on (default true).
func (c RulesConfig) HoldDigestOn() bool { return c.HoldDigest == nil || *c.HoldDigest }

// IntelConfig drives the header scanner that builds sender profiles (AGENT.md
// D19, D20, D28). The scan reads headers only, never bodies, and never sets
// \Seen.
type IntelConfig struct {
	// Enabled turns the scanner on. Pointer so an explicit false is kept.
	Enabled *bool `yaml:"enabled"`
	// ScanIntervalMinutes between scan ticks. Default 15.
	ScanIntervalMinutes int `yaml:"scan_interval_minutes"`
	// BackfillPerMinute caps messages scanned per minute across accounts. Default 600.
	BackfillPerMinute int `yaml:"backfill_per_minute"`
	// BatchSize is messages fetched per IMAP round trip. Default 200.
	BatchSize int `yaml:"batch_size"`
	// ExcludeFolders are skipped in addition to \Junk and \Drafts.
	ExcludeFolders []string `yaml:"exclude_folders"`
	// LLMRoles lets the classify model assign roles to senders the signals
	// leave unknown (gated like backfill enrichment). Pointer: default true.
	LLMRoles *bool `yaml:"llm_roles"`
	// LLMRolesPerTick caps model calls per scan tick. Default 20.
	LLMRolesPerTick int `yaml:"llm_roles_per_tick"`
	// KG builds the knowledge graph from the scan and the cache (D21).
	// Pointer: default true.
	KG *bool `yaml:"kg"`
	// KGStaleDays: a relationship without evidence for this long gets
	// valid_to set (it is history, not current). Default 365.
	KGStaleDays int `yaml:"kg_stale_days"`
	// KGLLM lets the classify model extract relations from recent
	// conversation bodies (gated like backfill enrichment). Default true.
	KGLLM *bool `yaml:"kg_llm"`
	// KGLLMPerTick caps extraction model calls per tick. Default 10.
	KGLLMPerTick int `yaml:"kg_llm_per_tick"`

	// Anomalies turns detection on (D22). Pointer: default true.
	Anomalies *bool `yaml:"anomalies"`
	// AnomalyLookbackDays: per-message checks only look at mail this recent. Default 7.
	AnomalyLookbackDays int `yaml:"anomaly_lookback_days"`
	// AnomalyAuthMinPasses: auth_failure needs at least this many earlier passes. Default 3.
	AnomalyAuthMinPasses int `yaml:"anomaly_auth_min_passes"`
	// AnomalySilenceMinMessages: silence only for senders with at least this many messages. Default 20.
	AnomalySilenceMinMessages int `yaml:"anomaly_silence_min_messages"`
	// AnomalySilenceMinDays: the shortest silence reported. Default 30.
	AnomalySilenceMinDays int `yaml:"anomaly_silence_min_days"`
	// AnomalySpikeMin and AnomalySpikeFactor: a volume spike is more than
	// max(min, factor × daily average) messages in 24 hours. Defaults 10 and 5.
	AnomalySpikeMin    int `yaml:"anomaly_spike_min"`
	AnomalySpikeFactor int `yaml:"anomaly_spike_factor"`
}

// AnomaliesOn reports whether anomaly detection runs (default true).
func (c IntelConfig) AnomaliesOn() bool { return c.Anomalies == nil || *c.Anomalies }

// KGOn reports whether the knowledge graph is built (default true).
func (c IntelConfig) KGOn() bool { return c.KG == nil || *c.KG }

// KGLLMOn reports whether model extraction from bodies is allowed (default true).
func (c IntelConfig) KGLLMOn() bool { return c.KGLLM == nil || *c.KGLLM }

// On reports whether the scanner runs (default true).
func (c IntelConfig) On() bool { return c.Enabled == nil || *c.Enabled }

// LLMRolesOn reports whether model-assigned roles are allowed (default true).
func (c IntelConfig) LLMRolesOn() bool { return c.LLMRoles == nil || *c.LLMRoles }

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
			Enabled:           true,
			OllamaURL:         "http://localhost:11434",
			EmbedModel:        "nomic-embed-text",
			LLMModel:          "qwen3:1.7b",
			BatchSize:         20,
			AutoSync:          true,
			Concurrency:       2,
			BackfillPerMinute: 30,
			MaxAttempts:       3,
			BackoffMaxSeconds: 300,
			Yield:             YieldConfig{Enabled: true, MaxForeignResidentGB: 8},
		},
		Sync: SyncConfig{
			IntervalMinutes:     15,
			FullSyncOnStart:     true,
			Folders:             []string{"INBOX", `\Sent`},
			WindowDays:          30,
			MaxMessageMB:        25,
			VacuumIntervalHours: 24,
		},
		Tools: ToolsConfig{
			AttachmentInlineKB: 64,
			AttachmentMaxMB:    25,
			ExportMaxMessages:  500,
			ExportMaxMB:        100,
		},
		Intel: IntelConfig{
			ScanIntervalMinutes: 15,
			BackfillPerMinute:   600,
			BatchSize:           200,
			LLMRolesPerTick:     20,
			KGStaleDays:         365,
			KGLLMPerTick:        10,

			AnomalyLookbackDays:       7,
			AnomalyAuthMinPasses:      3,
			AnomalySilenceMinMessages: 20,
			AnomalySilenceMinDays:     30,
			AnomalySpikeMin:           10,
			AnomalySpikeFactor:        5,
		},
		Rules: RulesConfig{HoldDigestHour: 8},
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
	if v := os.Getenv("IMAP_MCP_SERVER_AUTH_DISABLED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.Server.Auth.Disabled = b
		}
	}
	if v := os.Getenv("IMAP_MCP_SYNC_WINDOW_DAYS"); v != "" {
		if d, err := strconv.Atoi(v); err == nil {
			cfg.Sync.WindowDays = d
		}
	}
	if v := os.Getenv("IMAP_MCP_SYNC_MAX_MESSAGE_MB"); v != "" {
		if d, err := strconv.Atoi(v); err == nil {
			cfg.Sync.MaxMessageMB = d
		}
	}
	if v := os.Getenv("IMAP_MCP_SYNC_KEEP_FLAGGED"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.Sync.KeepFlagged = b
		}
	}
	if v := os.Getenv("IMAP_MCP_SYNC_VACUUM_INTERVAL_HOURS"); v != "" {
		if d, err := strconv.Atoi(v); err == nil {
			cfg.Sync.VacuumIntervalHours = d
		}
	}
	if v := os.Getenv("IMAP_MCP_SYNC_INTERVAL_MINUTES"); v != "" {
		if d, err := strconv.Atoi(v); err == nil {
			cfg.Sync.IntervalMinutes = d
		}
	}
	if v := os.Getenv("IMAP_MCP_DB_PATH"); v != "" {
		cfg.DB.Path = v
	}
	if v := os.Getenv("IMAP_MCP_DB_CACHE_PATH"); v != "" {
		cfg.DB.Cache.Path = v
	}
	if v := os.Getenv("IMAP_MCP_DB_ENCRYPTION_KEY"); v != "" {
		cfg.DB.EncryptionKey = v
	}
	if v := os.Getenv("IMAP_MCP_DB_CACHE_ENCRYPTION_KEY"); v != "" {
		cfg.DB.Cache.EncryptionKey = v
	}
	if v := os.Getenv("IMAP_MCP_OLLAMA_URL"); v != "" {
		cfg.Enrichment.OllamaURL = v
	}
	envInt := func(key string, dst *int) {
		if v := os.Getenv(key); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				*dst = n
			}
		}
	}
	envInt("IMAP_MCP_ENRICHMENT_CONCURRENCY", &cfg.Enrichment.Concurrency)
	envInt("IMAP_MCP_ENRICHMENT_BACKFILL_PER_MINUTE", &cfg.Enrichment.BackfillPerMinute)
	envInt("IMAP_MCP_ENRICHMENT_MAX_ATTEMPTS", &cfg.Enrichment.MaxAttempts)
	envInt("IMAP_MCP_ENRICHMENT_BACKOFF_MAX_SECONDS", &cfg.Enrichment.BackoffMaxSeconds)
	envInt("IMAP_MCP_TOOLS_ATTACHMENT_INLINE_KB", &cfg.Tools.AttachmentInlineKB)
	envInt("IMAP_MCP_TOOLS_ATTACHMENT_MAX_MB", &cfg.Tools.AttachmentMaxMB)
	envInt("IMAP_MCP_TOOLS_EXPORT_MAX_MESSAGES", &cfg.Tools.ExportMaxMessages)
	envInt("IMAP_MCP_TOOLS_EXPORT_MAX_MB", &cfg.Tools.ExportMaxMB)
	envInt("IMAP_MCP_INTELLIGENCE_SCAN_INTERVAL_MINUTES", &cfg.Intel.ScanIntervalMinutes)
	envInt("IMAP_MCP_INTELLIGENCE_BACKFILL_PER_MINUTE", &cfg.Intel.BackfillPerMinute)
	envInt("IMAP_MCP_INTELLIGENCE_BATCH_SIZE", &cfg.Intel.BatchSize)
	envInt("IMAP_MCP_INTELLIGENCE_LLM_ROLES_PER_TICK", &cfg.Intel.LLMRolesPerTick)
	envInt("IMAP_MCP_INTELLIGENCE_KG_STALE_DAYS", &cfg.Intel.KGStaleDays)
	envInt("IMAP_MCP_INTELLIGENCE_KG_LLM_PER_TICK", &cfg.Intel.KGLLMPerTick)
	envInt("IMAP_MCP_INTELLIGENCE_ANOMALY_LOOKBACK_DAYS", &cfg.Intel.AnomalyLookbackDays)
	envInt("IMAP_MCP_INTELLIGENCE_ANOMALY_AUTH_MIN_PASSES", &cfg.Intel.AnomalyAuthMinPasses)
	envInt("IMAP_MCP_INTELLIGENCE_ANOMALY_SILENCE_MIN_MESSAGES", &cfg.Intel.AnomalySilenceMinMessages)
	envInt("IMAP_MCP_INTELLIGENCE_ANOMALY_SILENCE_MIN_DAYS", &cfg.Intel.AnomalySilenceMinDays)
	envInt("IMAP_MCP_INTELLIGENCE_ANOMALY_SPIKE_MIN", &cfg.Intel.AnomalySpikeMin)
	envInt("IMAP_MCP_INTELLIGENCE_ANOMALY_SPIKE_FACTOR", &cfg.Intel.AnomalySpikeFactor)
	envInt("IMAP_MCP_RULES_HOLD_DIGEST_HOUR", &cfg.Rules.HoldDigestHour)
	for key, dst := range map[string]**bool{
		"IMAP_MCP_INTELLIGENCE_ENABLED":   &cfg.Intel.Enabled,
		"IMAP_MCP_INTELLIGENCE_LLM_ROLES": &cfg.Intel.LLMRoles,
		"IMAP_MCP_INTELLIGENCE_KG":        &cfg.Intel.KG,
		"IMAP_MCP_INTELLIGENCE_KG_LLM":    &cfg.Intel.KGLLM,
		"IMAP_MCP_INTELLIGENCE_ANOMALIES": &cfg.Intel.Anomalies,
		"IMAP_MCP_RULES_HOLD_DIGEST":      &cfg.Rules.HoldDigest,
	} {
		if v := os.Getenv(key); v != "" {
			if b, err := strconv.ParseBool(v); err == nil {
				*dst = &b
			}
		}
	}
	if v := os.Getenv("IMAP_MCP_ENRICHMENT_BACKFILL_WINDOW"); v != "" {
		cfg.Enrichment.BackfillWindow = v
	}
	if v := os.Getenv("IMAP_MCP_ENRICHMENT_CLASSIFY_PROVIDER"); v != "" {
		cfg.Enrichment.Classify.Provider = v
	}
	if v := os.Getenv("IMAP_MCP_ENRICHMENT_YIELD"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			cfg.Enrichment.Yield.Enabled = b
		}
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
	cfg.DB.Cache.Path = expand(cfg.DB.Cache.Path)
	if cfg.DB.Cache.Path == "" {
		cfg.DB.Cache.Path = filepath.Join(filepath.Dir(cfg.DB.Path), "cache.db")
	}
	cfg.WorkingDir = expand(cfg.WorkingDir)
	if cfg.Datawatch != nil {
		cfg.Datawatch.CAFile = expand(cfg.Datawatch.CAFile)
	}
	for i := range cfg.Accounts {
		cfg.Accounts[i].Auth.TokenFile = expand(cfg.Accounts[i].Auth.TokenFile)
		cfg.Accounts[i].Auth.ServiceAccountFile = expand(cfg.Accounts[i].Auth.ServiceAccountFile)
	}
}

func validate(cfg *Config) error {
	if cfg.Datawatch != nil {
		if _, err := cfg.Datawatch.Transport(); err != nil {
			return err
		}
	}
	if err := validateEnrichment(cfg); err != nil {
		return err
	}
	t := cfg.Tools
	if t.AttachmentInlineKB < 0 || t.AttachmentMaxMB < 1 || t.ExportMaxMessages < 1 || t.ExportMaxMB < 1 {
		return fmt.Errorf("tools: attachment_inline_kb must be >= 0; attachment_max_mb, export_max_messages and export_max_mb must be >= 1")
	}
	if in := cfg.Intel; in.ScanIntervalMinutes < 1 || in.BackfillPerMinute < 1 || in.BatchSize < 1 || in.BatchSize > 1000 || in.LLMRolesPerTick < 0 ||
		in.KGStaleDays < 1 || in.KGLLMPerTick < 0 || in.AnomalyLookbackDays < 1 || in.AnomalyAuthMinPasses < 1 ||
		in.AnomalySilenceMinMessages < 2 || in.AnomalySilenceMinDays < 1 || in.AnomalySpikeMin < 1 || in.AnomalySpikeFactor < 1 {
		return fmt.Errorf("intelligence: scan_interval_minutes, backfill_per_minute, kg_stale_days and the anomaly_* thresholds must be >= 1 (anomaly_silence_min_messages >= 2); batch_size 1-1000; llm_roles_per_tick, kg_llm_per_tick >= 0")
	}
	if h := cfg.Rules.HoldDigestHour; h < 0 || h > 23 {
		return fmt.Errorf("rules: hold_digest_hour must be 0-23")
	}
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

func validateEnrichment(cfg *Config) error {
	e := cfg.Enrichment
	switch e.ClassifyProvider() {
	case "ollama":
	case "datawatch":
		if cfg.Datawatch == nil || cfg.Datawatch.APIURL == "" {
			return fmt.Errorf("enrichment.classify.provider datawatch needs a datawatch block with api_url and token")
		}
		if e.Classify.DatawatchLLM == "" {
			return fmt.Errorf("enrichment.classify.datawatch_llm is required for provider datawatch")
		}
	default:
		return fmt.Errorf("enrichment.classify.provider must be ollama or datawatch, got %q", e.Classify.Provider)
	}
	if _, _, err := ParseWindow(e.BackfillWindow); err != nil {
		return fmt.Errorf("enrichment.backfill_window: %w", err)
	}
	return nil
}

// ParseWindow parses "HH:MM-HH:MM" into minutes after midnight. An empty
// string is valid and means "no restriction" (start == end == -1).
func ParseWindow(w string) (start, end int, err error) {
	if strings.TrimSpace(w) == "" {
		return -1, -1, nil
	}
	parts := strings.Split(w, "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("want HH:MM-HH:MM, got %q", w)
	}
	parse := func(s string) (int, error) {
		t, err := time.Parse("15:04", strings.TrimSpace(s))
		if err != nil {
			return 0, fmt.Errorf("bad time %q", s)
		}
		return t.Hour()*60 + t.Minute(), nil
	}
	if start, err = parse(parts[0]); err != nil {
		return
	}
	if end, err = parse(parts[1]); err != nil {
		return
	}
	if start == end {
		return 0, 0, fmt.Errorf("empty window %q", w)
	}
	return start, end, nil
}
