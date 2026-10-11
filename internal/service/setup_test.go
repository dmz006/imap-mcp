package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/dmz006/imap-mcp/internal/db"
)

// TestSetupCheck covers D52: the check finds the gaps production tuning
// found, and the digest reports each new finding once.
func TestSetupCheck(t *testing.T) {
	f := newHoldFixture(t)
	ctx := context.Background()
	f.s.cfg.Sync.Folders = []string{"INBOX"}
	st := f.d.StateSQL()
	if _, err := st.Exec(`INSERT INTO identities(address, status, updated_at) VALUES('me@work.example','candidate',1)`); err != nil {
		t.Fatal(err)
	}
	id, err := f.s.CreateRule(ctx, &db.Rule{Name: "never", Active: true, Conditions: db.RuleConditions{From: "nobody.example"},
		Actions: []db.RuleAction{{Type: "trash"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Exec(`UPDATE rules SET created_at = ? WHERE id = ?`, time.Now().AddDate(0, 0, -60).Unix(), id); err != nil {
		t.Fatal(err)
	}
	rep, err := f.s.SetupCheck(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, fd := range rep.Findings {
		ids[fd.ID] = true
		if fd.What == "" || fd.Why == "" || fd.Fix == "" {
			t.Errorf("incomplete finding %+v", fd)
		}
	}
	for _, want := range []string{"sent_not_synced", "identities_pending", "no_hold_rule", "rules_never_matched", "history_incomplete"} {
		if !ids[want] {
			t.Errorf("missing finding %s in %+v", want, rep.Findings)
		}
	}

	// The digest reports new findings once.
	now := time.Date(2026, 10, 10, 9, 0, 0, 0, time.Local)
	if err := f.s.sendHoldDigests(now); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "INBOX"); n != 1 {
		t.Fatalf("INBOX = %d, want the digest with the Setup section", n)
	}
	fresh, err := f.s.newFindings(ctx)
	if err != nil || len(fresh) != 0 {
		t.Errorf("findings reported again: %+v, %v", fresh, err)
	}
	msg := string(digestMessage("user@example.com", "test", nil, ReplyList{}, digestLearning{setup: rep.Findings[:1]}, now))
	if !strings.Contains(msg, "Setup: 1 new finding") || !strings.Contains(msg, "Fix:") {
		t.Errorf("digest body:\n%s", msg)
	}
}

// TestImportRulePack covers D52: packs import inactive, skip duplicates, and
// need their folder.
func TestImportRulePack(t *testing.T) {
	f := newHoldFixture(t)
	ctx := context.Background()
	res, err := f.s.ImportRulePack(ctx, "test", "bounces", "")
	if err != nil || len(res.Created) != 3 {
		t.Fatalf("import bounces = %+v, %v", res, err)
	}
	if again, err := f.s.ImportRulePack(ctx, "test", "bounces", ""); err != nil || len(again.Created) != 0 || len(again.Skipped) != 3 {
		t.Errorf("re-import = %+v, %v", again, err)
	}
	if _, err := f.s.ImportRulePack(ctx, "test", "dmarc-reports", ""); err == nil {
		t.Error("imported a move pack into a folder that does not exist")
	}
	res, err = f.s.ImportRulePack(ctx, "test", "dmarc-reports", "Held")
	if err != nil || len(res.Created) != 4 {
		t.Fatalf("import dmarc-reports = %+v, %v", res, err)
	}
	rules, _ := f.s.ListRules(ctx)
	for _, r := range rules {
		if r.Active {
			t.Errorf("imported rule active: %+v", r)
		}
		if strings.Contains(r.Name, "dmarc-reports") && r.Actions[0].Dest != "Held" {
			t.Errorf("dest not applied: %+v", r)
		}
	}
	if _, err := f.s.ImportRulePack(ctx, "test", "nope", ""); err == nil {
		t.Error("unknown pack imported")
	}
}
