package config

import "testing"

func TestToolsDefaultsEnvAndValidation(t *testing.T) {
	c := defaults()
	if c.Tools != (ToolsConfig{AttachmentInlineKB: 64, AttachmentMaxMB: 25, ExportMaxMessages: 500, ExportMaxMB: 100}) {
		t.Fatalf("defaults = %+v", c.Tools)
	}
	t.Setenv("IMAP_MCP_TOOLS_ATTACHMENT_INLINE_KB", "0")
	t.Setenv("IMAP_MCP_TOOLS_EXPORT_MAX_MESSAGES", "50")
	applyEnvOverrides(c)
	if c.Tools.AttachmentInlineKB != 0 || c.Tools.ExportMaxMessages != 50 {
		t.Fatalf("env overrides = %+v", c.Tools)
	}
	c.Accounts = []AccountConfig{{Name: "a", Default: true, IMAP: IMAPConfig{Host: "imap.example.com"}, Auth: AuthConfig{Type: "plain", Username: "u", Password: "p"}}}
	if err := validate(c); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	for _, bad := range []func(*ToolsConfig){
		func(t *ToolsConfig) { t.AttachmentInlineKB = -1 },
		func(t *ToolsConfig) { t.AttachmentMaxMB = 0 },
		func(t *ToolsConfig) { t.ExportMaxMessages = 0 },
		func(t *ToolsConfig) { t.ExportMaxMB = -5 },
	} {
		cc := *c
		bad(&cc.Tools)
		if err := validate(&cc); err == nil {
			t.Errorf("invalid tools config accepted: %+v", cc.Tools)
		}
	}
}

func TestIntelDefaultsEnvAndValidation(t *testing.T) {
	c := defaults()
	if !c.Intel.On() || !c.Intel.LLMRolesOn() || c.Intel.BackfillPerMinute != 600 || c.Intel.BatchSize != 200 {
		t.Fatalf("defaults = %+v", c.Intel)
	}
	t.Setenv("IMAP_MCP_INTELLIGENCE_ENABLED", "false")
	t.Setenv("IMAP_MCP_INTELLIGENCE_LLM_ROLES", "false")
	t.Setenv("IMAP_MCP_INTELLIGENCE_BATCH_SIZE", "50")
	applyEnvOverrides(c)
	if c.Intel.On() || c.Intel.LLMRolesOn() || c.Intel.BatchSize != 50 {
		t.Fatalf("env = %+v", c.Intel)
	}
	c.Accounts = []AccountConfig{{Name: "a", Default: true, IMAP: IMAPConfig{Host: "imap.example.com"}, Auth: AuthConfig{Type: "plain", Username: "u", Password: "p"}}}
	if err := validate(c); err != nil {
		t.Fatal(err)
	}
	c.Intel.BatchSize = 5000
	if validate(c) == nil {
		t.Error("batch_size over 1000 accepted")
	}
}
