package service

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/testutil/imaptest"
)

// TestLearnFromMoves covers Q2 (D34–D36, D46, D47): the discard ratio over all
// history, the exclusions, domain rules (never for webmail, never over a
// correspondent), the mirrored action, dismissing, auto-creating, and that a
// deleted learned rule is never re-created.
func TestLearnFromMoves(t *testing.T) {
	srv := imaptest.Start(t, []string{"Held", "Trash", "Junk"})
	cfg := &config.Config{Accounts: []config.AccountConfig{srv.Account("test")}}
	b := bus.New()
	pool := imaptest.Pool(t, cfg, b)
	dir := t.TempDir()
	d, err := db.Open(db.Options{Path: filepath.Join(dir, "imap.db")}, db.Options{Path: filepath.Join(dir, "cache.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	s := New(cfg, pool, d, nil, nil)
	ctx := context.Background()
	st := d.StateSQL()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := st.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	hash := int64(1000)
	sender := func(addr string, sent, trusted int, folders ...string) {
		domain := addr[strings.LastIndex(addr, "@")+1:]
		r, err := st.Exec(`INSERT INTO senders(address, domain, sent_count, trusted) VALUES(?,?,?,?)`, addr, domain, sent, trusted)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := r.LastInsertId()
		for _, f := range folders {
			hash++
			exec(`INSERT INTO intel_messages(account, msg_hash, date, sender_id, folder) VALUES('test',?,1,?,?)`, hash, id, f)
		}
	}
	tr := []string{"Trash", "Trash", "Trash"}
	sender("spam1@bad.example", 0, 0, "Trash", "Trash", "Trash", "Trash")
	sender("spam2@bad.example", 0, 0, tr...)
	sender("junker@junk.example", 0, 0, "Junk", "Junk", "Junk", "Junk", "INBOX")
	sender("one@gmail.com", 0, 0, tr...)
	sender("two@gmail.com", 0, 0, tr...)
	sender("spama@mixed.example", 0, 0, tr...)
	sender("spamb@mixed.example", 0, 0, tr...)
	sender("pal@mixed.example", 2, 0, "INBOX")
	sender("friend@example.org", 3, 0, tr...)                                      // a correspondent
	sender("low@ratio.example", 0, 0, "Trash", "Trash", "Trash", "INBOX", "INBOX") // 0.6
	sender("ruled@covered.example", 0, 0, tr...)                                   // a rule covers it
	sender("held@held.example", 0, 0, tr...)                                       // held by new_sender
	sender("trusted@t.example", 0, 1, tr...)
	sender("moved@rm.example", 0, 0, tr...)  // a rule moved these
	sender("doc@named.example", 0, 0, tr...) // a display-name rule covers it
	exec(`UPDATE senders SET name = 'Dr. Martin' WHERE address = 'doc@named.example'`)
	if _, err := s.CreateRule(ctx, &db.Rule{Name: "by-name", Active: true, Conditions: db.RuleConditions{From: "Dr. Martin"},
		Actions: []db.RuleAction{{Type: "trash"}}}); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO rule_moves(account, msg_hash, rule_id, dest, moved_at) SELECT 'test', m.msg_hash, 1, 'Trash', ? FROM intel_messages m
		JOIN senders s ON s.id = m.sender_id WHERE s.address = 'moved@rm.example'`, time.Now().Unix())
	exec(`INSERT INTO held_messages(account, msg_hash, sender, held_at) VALUES('test', 1, 'held@held.example', 1)`)
	if _, err := s.CreateRule(ctx, &db.Rule{Name: "covered", Active: true, Conditions: db.RuleConditions{From: "covered.example"},
		Actions: []db.RuleAction{{Type: "trash"}}}); err != nil {
		t.Fatal(err)
	}

	targets := func(l SuggestionList) []string {
		var out []string
		for _, sg := range l.Suggestions {
			out = append(out, sg.Target+"="+sg.Action+sg.Dest)
		}
		sort.Strings(out)
		return out
	}
	list, err := s.SuggestRules(ctx, SuggestParams{})
	if err != nil {
		t.Fatal(err)
	}
	want := "@bad.example=trash junker@junk.example=moveJunk one@gmail.com=trash spama@mixed.example=trash spamb@mixed.example=trash two@gmail.com=trash"
	if got := strings.Join(targets(list), " "); got != want || list.Mode != "suggest" {
		t.Fatalf("suggestions = %s (mode %s)\nwant %s", got, list.Mode, want)
	}
	for _, sg := range list.Suggestions {
		if sg.Target == "@bad.example" && (sg.Received != 7 || sg.Discarded != 7 || len(sg.Addresses) != 2 || sg.Rule.Active || sg.Rule.Conditions.From != "@bad.example") {
			t.Errorf("domain suggestion = %+v", sg)
		}
	}

	if _, err := s.DismissSuggestion(ctx, "test", "@bad.example"); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.SuggestRules(ctx, SuggestParams{}); len(l.Suggestions) != 5 {
		t.Fatalf("after dismissing the domain: %v", targets(l))
	}

	// Active mode: the hourly pass creates the rules and announces them.
	var events []bus.Event
	b.Subscribe(bus.EventRuleSuggested, func(e bus.Event) { events = append(events, e) })
	s.cfg.Rules.Learn.Mode = config.LearnActive
	if err := s.learnRun(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	rules, _ := s.ListRules(ctx)
	learned := 0
	var firstID int64
	for _, r := range rules {
		if strings.HasPrefix(r.Name, learnedRulePrefix) {
			learned++
			if !r.Active {
				t.Errorf("active mode created an inactive rule: %+v", r)
			}
			if firstID == 0 {
				firstID = r.ID
			}
		}
	}
	if learned != 5 || len(events) != 1 || events[0].Payload.(map[string]any)["created"] != 5 {
		t.Fatalf("learned rules = %d, events = %+v", learned, events)
	}
	if err := s.learnRun(ctx, time.Now()); err != nil || len(events) != 1 {
		t.Errorf("second pass re-announced or failed: %d events, %v", len(events), err)
	}
	// Deleting a learned rule means "no": it is never created again.
	if err := s.DeleteRule(ctx, firstID); err != nil {
		t.Fatal(err)
	}
	if err := s.learnRun(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if rules, _ := s.ListRules(ctx); len(rules) != 6 { // covered + by-name + 4 learned
		t.Errorf("a deleted learned rule came back: %d rules", len(rules))
	}
}

// TestRuleMovesRecorded (D34): messages a rule trashes are remembered, so
// they never count as the owner's discards.
func TestRuleMovesRecorded(t *testing.T) {
	f := newHoldFixture(t)
	f.srv.Append(t, "INBOX", raw("rm1@x", "Promo <promo@shop.example>", imaptest.Username, "Sale"), time.Now())
	ctx := context.Background()
	id, err := f.s.CreateRule(ctx, &db.Rule{Name: "promo", Active: true, Conditions: db.RuleConditions{From: "shop.example"},
		Actions: []db.RuleAction{{Type: "move", Dest: "Held"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.RunActiveRules(id, false); err != nil {
		t.Fatal(err)
	}
	var n int
	f.d.StateSQL().QueryRow(`SELECT count(*) FROM rule_moves WHERE rule_id = ? AND dest = 'Held'`, id).Scan(&n) //nolint:errcheck
	if n != 1 {
		t.Errorf("rule_moves rows = %d, want 1", n)
	}
}
