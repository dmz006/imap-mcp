package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/testutil/imaptest"
)

func TestRunRulesPublishesRuleFired(t *testing.T) {
	srv := imaptest.Start(t, []string{"Archive"})
	srv.Append(t, "INBOX", imaptest.Plain("m1@example.com", "alice@example.com", "Invoice 1", "pay me"), time.Now())
	cfg := &config.Config{Accounts: []config.AccountConfig{srv.Account("test")}}
	b := bus.New()
	var got []bus.Event
	b.Subscribe(bus.EventRuleFired, func(e bus.Event) { got = append(got, e) }) // Publish is synchronous
	pool := imaptest.Pool(t, cfg, b)
	dir := t.TempDir()
	d, err := db.Open(db.Options{Path: filepath.Join(dir, "imap.db")}, db.Options{Path: filepath.Join(dir, "cache.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	s := New(cfg, pool, d, nil, nil)
	id, err := s.CreateRule(context.Background(), &db.Rule{Name: "archive-alice", Active: true,
		Conditions: db.RuleConditions{From: "alice"}, Actions: []db.RuleAction{{Type: "move", Dest: "Archive"}}})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.RunActiveRules(0, true); err != nil || len(got) != 0 {
		t.Fatalf("dry run fired %d events (%v)", len(got), err)
	}
	if _, err := s.RunActiveRules(0, false); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("rule.fired events = %d", len(got))
	}
	p := got[0].Payload.(map[string]any)
	if p["rule_id"] != id || p["matched"] != 1 || p["action"] != "move" {
		t.Errorf("payload = %v", p)
	}
	if _, err := s.RunActiveRules(0, false); err != nil || len(got) != 1 {
		t.Errorf("no-match run fired an event: %d", len(got))
	}
}
