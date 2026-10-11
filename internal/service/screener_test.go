package service

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/intel"
	"github.com/dmz006/imap-mcp/internal/testutil/imaptest"
)

// TestScreener covers Q5 (D42, D43, D61, D62): listing held senders, a
// preview that changes nothing, approve to a feed with a routing rule,
// block by domain (address only next to a correspondent), and digest
// replies in token mode, including rejected ones.
func TestScreener(t *testing.T) {
	srv := imaptest.Start(t, []string{"Held", "Trash"})
	now := time.Now()
	held := []struct{ id, from string }{
		{"n1@shop.example", "news@shop.example"},
		{"s1@bad.example", "spam@bad.example"},
		{"s2@bad.example", "spam@bad.example"},
		{"c1@corp.example", "cold@corp.example"},
		{"k1@kept.example", "keep@kept.example"},
	}
	for _, h := range held {
		srv.Append(t, "Held", imaptest.Plain(h.id, h.from, "hello", "body"), now)
	}
	srv.Append(t, "INBOX", imaptest.Plain("i1@bad.example", "other@bad.example", "more", "body"), now)

	cfg := &config.Config{Accounts: []config.AccountConfig{srv.Account("test")}}
	b := bus.New()
	var decided atomic.Int32
	b.Subscribe(bus.EventScreenerDecided, func(bus.Event) { decided.Add(1) })
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
	for i, h := range held {
		if _, err := st.Exec(`INSERT INTO held_messages(account, msg_hash, message_ref, sender, subject, reasons, folder, held_at) VALUES('test',?,?,?,?,?,?,?)`,
			intel.MsgHash(h.id), "<"+h.id+">", h.from, "hello", "bulk", "Held", now.Add(time.Duration(i)*time.Second).Unix()); err != nil {
			t.Fatal(err)
		}
	}
	// Someone the owner writes to at corp.example: a block there is the address only.
	if _, err := st.Exec(`INSERT INTO senders(address, domain, sent_count) VALUES('pal@corp.example','corp.example',3)`); err != nil {
		t.Fatal(err)
	}

	list, err := s.HeldSenders(ctx, "")
	if err != nil || list.Count != 4 || list.Senders[1].Sender != "spam@bad.example" || list.Senders[1].Messages != 2 || list.Senders[1].Number != 2 {
		t.Fatalf("held = %+v (%v)", list, err)
	}

	decisions := []ScreenDecision{
		{Sender: "news@shop.example", Action: "approve", Dest: "feed"},
		{Sender: "Spam <spam@bad.example>", Action: "block"},
		{Sender: "cold@corp.example", Action: "block"},
	}
	prev, err := s.Screen(ctx, ScreenParams{Decisions: decisions})
	if err != nil {
		t.Fatal(err)
	}
	if !prev.DryRun || !strings.Contains(prev.Run, `"confirm":true`) || prev.Outcomes[0].Rule.Dest != "Feed" || prev.Outcomes[0].Rule.Active ||
		prev.Outcomes[1].Rule.From != "bad.example" || prev.Outcomes[1].Rule.Matches != 1 || prev.Outcomes[2].Rule.From != "cold@corp.example" {
		t.Fatalf("preview = %+v", prev)
	}
	if again, _ := s.HeldSenders(ctx, ""); again.Count != 4 {
		t.Fatalf("a preview changed something: %+v", again)
	}

	res, err := s.Screen(ctx, ScreenParams{Decisions: decisions, Confirm: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Outcomes[0].Moved != 1 || res.Outcomes[0].Rule.ID == 0 || res.Outcomes[1].Moved != 2 || res.Outcomes[1].Folder != "Trash" || decided.Load() != 1 {
		t.Fatalf("applied = %+v, events %d", res, decided.Load())
	}
	for folder, want := range map[string]uint32{"Feed": 1, "Trash": 3, "Held": 1} {
		if p, err := s.ListMessages(ctx, ListMessagesParams{Folder: folder}); err != nil || p.Total != want {
			t.Errorf("%s holds %d, want %d (%v)", folder, p.Total, want, err)
		}
	}
	var trusted int
	_ = st.QueryRow(`SELECT trusted FROM senders WHERE address = 'news@shop.example'`).Scan(&trusted)
	rules, _ := d.Rules.List()
	if trusted != 1 || len(rules) != 3 {
		t.Fatalf("trusted=%d rules=%d", trusted, len(rules))
	}
	if left, _ := s.HeldSenders(ctx, ""); left.Count != 1 || left.Senders[0].Sender != "keep@kept.example" {
		t.Fatalf("left = %+v", left)
	}

	// Token-mode digest replies (D42).
	ref := "<hold-digest-test-1@imap-mcp.invalid>"
	if _, err := st.Exec(`INSERT INTO screen_digests(account, message_ref, token, numbering, created_at) VALUES('test',?,?,?,?)`,
		ref, "SCR-AAAA-BBBB-CCCC", `["keep@kept.example"]`, now.Unix()); err != nil {
		t.Fatal(err)
	}
	reply := func(id, from, auth, body string) {
		srv.Append(t, "INBOX", "From: "+from+"\r\nTo: "+imaptest.Username+"\r\nSubject: Re: digest\r\nMessage-ID: <"+id+">\r\nIn-Reply-To: "+ref+
			"\r\nAuthentication-Results: mx.example.com; "+auth+"\r\nContent-Type: text/plain\r\n\r\n"+body+"\r\n", time.Now())
	}
	reply("r1@example.com", "someone@evil.example", "dkim=pass; dmarc=pass", "approve 1\r\n> token SCR-AAAA-BBBB-CCCC")
	reply("r2@example.com", imaptest.Username, "dkim=fail; dmarc=fail", "approve 1\r\n> token SCR-AAAA-BBBB-CCCC")
	reply("r3@example.com", imaptest.Username, "dkim=pass; dmarc=pass", "approve 1 receipts\r\n\r\n> token SCR-AAAA-BBBB-CCCC")
	if err := s.ScreenReplyTick(ctx); err != nil {
		t.Fatal(err)
	}
	statuses := map[string]int{}
	rows, _ := st.Query(`SELECT status FROM screen_replies`)
	for rows.Next() {
		var x string
		_ = rows.Scan(&x)
		statuses[x]++
	}
	rows.Close()
	// The forged sender is not even read (search is by the owner's address).
	if statuses["ok"] != 1 || statuses["no dkim=pass and dmarc=pass in Authentication-Results"] != 1 {
		t.Fatalf("reply statuses = %v", statuses)
	}
	if p, _ := s.ListMessages(ctx, ListMessagesParams{Folder: "Receipts"}); p.Total != 1 {
		t.Fatalf("receipts holds %d", p.Total)
	}
	page, _ := s.ListMessages(ctx, ListMessagesParams{Folder: "INBOX"})
	confirm := 0
	for _, m := range page.Messages {
		if strings.HasPrefix(m.Subject, "Screener: 1 approved") {
			confirm++
		}
	}
	if confirm != 1 {
		t.Fatalf("confirmations = %d", confirm)
	}
	// A second valid reply is rejected: the token is single-use.
	reply("r4@example.com", imaptest.Username, "dkim=pass; dmarc=pass", "release all\r\n> token SCR-AAAA-BBBB-CCCC")
	if err := s.ScreenReplyTick(ctx); err != nil {
		t.Fatal(err)
	}
	var used int
	_ = st.QueryRow(`SELECT count(*) FROM screen_replies WHERE status = 'token already used'`).Scan(&used)
	if used != 1 {
		t.Fatalf("reused token not rejected")
	}
}

func TestParseScreenCommands(t *testing.T) {
	nums := []string{"a@x.example", "b@y.example", "c@z.example"}
	ds, bad := ParseScreenCommands("Approve 1 feed\r\nblock 2,3\r\nfrobnicate\r\n> approve 3\r\n-- \r\nsig approve 1", nums)
	if len(ds) != 3 || ds[0] != (ScreenDecision{Sender: "a@x.example", Action: "approve", Dest: "feed"}) ||
		ds[1].Action != "block" || ds[2].Sender != "c@z.example" || len(bad) != 1 || bad[0] != "frobnicate" {
		t.Fatalf("ds=%+v bad=%v", ds, bad)
	}
	ds, _ = ParseScreenCommands("release all", nums)
	if len(ds) != 3 || ds[2].Dest != "inbox" {
		t.Fatalf("release all = %+v", ds)
	}
	if ds, bad := ParseScreenCommands("approve 9\nblock x", nums); len(ds) != 0 || len(bad) != 2 {
		t.Fatalf("out of range: %+v %v", ds, bad)
	}
}

func TestDigestScreenSection(t *testing.T) {
	held := []heldRow{{sender: "spam@bad.example", subject: "hi", reasons: "bulk", folder: "Held", heldAt: time.Now().Unix()}}
	senders := []HeldSender{{Number: 1, Sender: "old@x.example", Messages: 2}, {Number: 2, Sender: "spam@bad.example", Messages: 1}}
	for mode, want := range map[string]string{
		config.ScreenEmailOff:   "screen_senders (MCP), POST /api/held/test/decide",
		config.ScreenEmailToken: "token SCR-TEST",
		config.ScreenEmailHMAC:  "verb screen",
	} {
		msg := string(digestMessage("me@example.com", "test", held, ReplyList{}, digestLearning{screen: digestScreen{mode: mode, token: "SCR-TEST", account: "test", senders: senders}}, time.Now()))
		for _, w := range []string{"[2] ", "Still held from earlier: [1] old@x.example (2)", want} {
			if !strings.Contains(msg, w) {
				t.Errorf("mode %s: digest lacks %q", mode, w)
			}
		}
		if mode != config.ScreenEmailToken && strings.Contains(msg, "SCR-TEST") {
			t.Errorf("mode %s shows the token", mode)
		}
	}
}
