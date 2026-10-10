package service

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/intel"
	"github.com/dmz006/imap-mcp/internal/testutil/imaptest"
)

// raw builds a test message with extra header lines.
func raw(id, from, to, subject string, extra ...string) string {
	h := "Message-ID: <" + id + ">\r\nFrom: " + from + "\r\n"
	if to != "" {
		h += "To: " + to + "\r\n"
	}
	h += "Subject: " + subject + "\r\nDate: " + time.Now().Format(time.RFC1123Z) + "\r\n"
	for _, e := range extra {
		h += e + "\r\n"
	}
	return h + "\r\nbody\r\n"
}

type holdFixture struct {
	srv *imaptest.Server
	s   *Service
	d   *db.DB
	b   *bus.Bus
}

func newHoldFixture(t *testing.T) *holdFixture {
	t.Helper()
	srv := imaptest.Start(t, []string{"Held"})
	cfg := &config.Config{Accounts: []config.AccountConfig{srv.Account("test")}}
	b := bus.New()
	pool := imaptest.Pool(t, cfg, b)
	dir := t.TempDir()
	d, err := db.Open(db.Options{Path: filepath.Join(dir, "imap.db")}, db.Options{Path: filepath.Join(dir, "cache.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return &holdFixture{srv: srv, s: New(cfg, pool, d, nil, nil), d: d, b: b}
}

func (f *holdFixture) count(t *testing.T, folder string) int {
	t.Helper()
	st, err := f.srv.User.Status(folder, &imaplib.StatusOptions{NumMessages: true})
	if err != nil {
		t.Fatal(err)
	}
	return int(*st.NumMessages)
}

// TestNewSenderHold covers D30: new senders are held only on bulk or scam
// signals; real first contacts stay; moving a held message back trusts its
// sender; nothing is matched until the history scan is complete.
func TestNewSenderHold(t *testing.T) {
	f := newHoldFixture(t)
	me := imaptest.Username
	now := time.Now()
	for _, m := range []string{
		raw("h1@x", "QuickBooks <info@fakebooks.example>", me, "Negative feedback"),                            // brand impersonation: held
		raw("h2@x", "Deals <deals@shop5x.example>", me, "Sale", "List-Unsubscribe: <mailto:u@shop5x.example>"), // bulk + throwaway: held
		raw("h3@x", "Promo <promo@single.example>", me, "News", "List-Unsubscribe: <mailto:u@single.example>"), // one signal: hall decides
		raw("k1@x", "Jane Roe <jane@newco.example>", me, "Hello from Jane"),                                    // real first contact: stays
		raw("k2@x", "PayPal <bob@other.example>", me, "Re: your note", "In-Reply-To: <sent1@example.com>"),     // replies to your mail: stays
		raw("k3@x", "Kim <kim@intro.example>", "friend@example.org", "Intro", "Cc: "+me),                       // copies a contact: stays
		raw("k4@x", "QuickBooks <friend@example.org>", me, "lunch?"),                                           // known sender: stays
		raw("k5@x", "Undisclosed <x@nobody.example>", "", "hi"),                                                // one signal, no hall: stays for now
	} {
		f.srv.Append(t, "INBOX", m, now)
	}
	st := f.d.StateSQL()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := st.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO senders(address, domain, sent_count, first_seen) VALUES('friend@example.org','example.org',3,?)`, now.Unix())
	exec(`INSERT INTO intel_messages(account, msg_hash, date, outgoing) VALUES('test',?,?,1)`, intel.MsgHash("sent1@example.com"), now.Unix())
	exec(`INSERT INTO intel_scan(account, folder, completed_at) VALUES('test','INBOX',NULL)`)

	ctx := context.Background()
	id, err := f.s.CreateRule(ctx, &db.Rule{Name: "hold-new", Active: true,
		Conditions: db.RuleConditions{NewSender: true}, Actions: []db.RuleAction{{Type: "move", Dest: "Held"}}})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := f.s.TestRule(ctx, id); err != nil || res.Matched != 0 || !strings.Contains(res.Error, "not complete") {
		t.Fatalf("incomplete scan: %+v, %v", res, err)
	}
	exec(`UPDATE intel_scan SET completed_at = ?`, now.Unix())

	if res, _ := f.s.TestRule(ctx, id); res.Matched != 2 || res.Error != "" || len(res.Preview) != 2 || res.Preview[0].Reasons == "" {
		t.Fatalf("dry run = %+v, want 2 (h1, h2) with reasons", res)
	}
	// The model classifies h3 as a newsletter: one signal plus a bulk hall holds it.
	if _, err := f.d.SQL().Exec(`INSERT INTO messages(account, folder, uid, from_addr, date, message_id, hall) VALUES('test','INBOX',99,'promo@single.example',1,'h3@x','newsletter')`); err != nil {
		t.Fatal(err)
	}
	if res, _ := f.s.TestRule(ctx, id); res.Matched != 3 {
		t.Fatalf("after hall = %+v, want 3", res)
	}
	if _, err := f.s.RunActiveRules(id, false); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "Held"); n != 3 {
		t.Fatalf("Held = %d, want 3", n)
	}
	var reasons string
	st.QueryRow(`SELECT reasons FROM held_messages WHERE message_ref = 'h1@x'`).Scan(&reasons) //nolint:errcheck
	if !strings.Contains(reasons, "brand") {
		t.Errorf("h1 reasons = %q", reasons)
	}

	// The owner moves h1 back: it stays, and its sender is trusted from now on.
	f.srv.Append(t, "INBOX", raw("h1@x", "QuickBooks <info@fakebooks.example>", me, "Negative feedback"), now)
	f.srv.Append(t, "INBOX", raw("h1b@x", "QuickBooks <info@fakebooks.example>", me, "Another"), now)
	if res, _ := f.s.TestRule(ctx, id); res.Matched != 0 {
		t.Errorf("released message or trusted sender matched: %+v", res)
	}
	if _, err := f.s.RunActiveRules(id, false); err != nil {
		t.Fatal(err)
	}
	var trusted int
	st.QueryRow(`SELECT trusted FROM senders WHERE address = 'info@fakebooks.example'`).Scan(&trusted) //nolint:errcheck
	if trusted != 1 {
		t.Error("released sender not trusted")
	}
	if res, _ := f.s.TestRule(ctx, id); res.Matched != 0 {
		t.Errorf("trusted sender held again: %+v", res)
	}
}

// TestHoldDigest covers D31: one digest per day, APPENDed to INBOX, with a
// hold.digest event that carries only the account and count.
func TestHoldDigest(t *testing.T) {
	f := newHoldFixture(t)
	var events []bus.Event
	f.b.Subscribe(bus.EventHoldDigest, func(e bus.Event) { events = append(events, e) })
	now := time.Date(2026, 10, 10, 9, 0, 0, 0, time.Local)
	for i, ref := range []string{"a@x", "b@x"} {
		if _, err := f.d.StateSQL().Exec(`INSERT INTO held_messages(account, msg_hash, message_ref, sender, subject, reasons, folder, held_at)
			VALUES('test',?,?,'spam@bad.example','Win big','bulk mail','Held',?)`, i+1, ref, now.Add(-time.Hour).Unix()); err != nil {
			t.Fatal(err)
		}
	}
	f.s.cfg.Rules.HoldDigestHour = 10
	if err := f.s.sendHoldDigests(now); err != nil || f.count(t, "INBOX") != 0 {
		t.Fatalf("digest before its hour: %v", err)
	}
	f.s.cfg.Rules.HoldDigestHour = 8
	if err := f.s.sendHoldDigests(now); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "INBOX"); n != 1 {
		t.Fatalf("INBOX = %d, want the digest", n)
	}
	if len(events) != 1 || len(events[0].Payload.(map[string]any)) != 2 || events[0].Payload.(map[string]any)["held"] != 2 {
		t.Fatalf("events = %+v", events)
	}
	if st, err := f.s.HoldStatus(); err != nil || st.Held != 2 || st.AwaitingDigest != 0 || st.LastDigest == "" || st.HoldDigestHour != 8 {
		t.Errorf("hold status after digest = %+v, %v", st, err)
	}
	// Same day, nothing new: no second digest.
	if err := f.s.sendHoldDigests(now.Add(time.Hour)); err != nil || f.count(t, "INBOX") != 1 {
		t.Errorf("second digest the same day: %v", err)
	}
	msg := string(holdDigestMessage("user@example.com", "test", []heldRow{{sender: "spam@bad.example", subject: "Win\r\nbig", reasons: "bulk mail", folder: "Held", heldAt: now.Unix()}}, now))
	if !strings.Contains(msg, "spam@bad.example") || !strings.Contains(msg, "Move a message back") || strings.Contains(msg, "Win\r\nbig") {
		t.Errorf("digest body:\n%s", msg)
	}
}

func TestNewSenderValidation(t *testing.T) {
	move := []db.RuleAction{{Type: "move", Dest: "Held"}}
	if err := ValidateRule(&db.Rule{Name: "a", Actions: move, Conditions: db.RuleConditions{NewSender: true}}); err != nil {
		t.Errorf("new_sender alone should be valid: %v", err)
	}
	if err := ValidateRule(&db.Rule{Name: "b", Actions: move, Conditions: db.RuleConditions{From: "x", NewSenderDays: 7}}); err == nil {
		t.Error("new_sender_days without new_sender accepted")
	}
	off := false
	s := &Service{cfg: &config.Config{Intel: config.IntelConfig{Enabled: &off}}}
	if _, err := s.newSenderGate("test", 0); err == nil {
		t.Error("new_sender allowed with intelligence off")
	}
	for d, want := range map[string]bool{"ctka5jyx.com": true, "norqenix68.pro": true, "bpcarre.shop": true, "example.com": false, "acme-supplies.org": false} {
		if throwawayDomain(d) != want {
			t.Errorf("throwawayDomain(%s) != %v", d, want)
		}
	}
}

// TestHallIgnoresPlaceholders: a stored hall that is a copied placeholder
// counts as not classified, so a one-signal message stays.
func TestHallIgnoresPlaceholders(t *testing.T) {
	f := newHoldFixture(t)
	g := &newSenderGate{account: "test", cache: f.d.SQL()}
	for id, hall := range map[string]string{"p1@x": "project or context", "p2@x": "<Newsletter>", "p3@x": "alert"} {
		if _, err := f.d.SQL().Exec(`INSERT INTO messages(account, folder, uid, from_addr, date, message_id, hall) VALUES('test','INBOX',?,'a@b.example',1,?,?)`, len(id)+int(id[1]), id, hall); err != nil {
			t.Fatal(err)
		}
	}
	// run-rules without the cache: everything counts as not classified.
	if got, err := (&newSenderGate{}).hall("p3@x"); err != nil || got != "" {
		t.Errorf("hall without cache = %q, %v", got, err)
	}
	for id, want := range map[string]string{"p1@x": "", "p2@x": "newsletter", "p3@x": "alert", "none@x": ""} {
		if got, err := g.hall(id); err != nil || got != want {
			t.Errorf("hall(%s) = %q, %v; want %q", id, got, err, want)
		}
	}
}

func TestImpersonation(t *testing.T) {
	g := &newSenderGate{ownDomains: map[string]bool{"example.com": true}, ownLabels: []string{"example"}}
	for _, c := range []struct {
		name, domain string
		flagged      bool
	}{
		{"amazon web services", "amazonaws.com", false},
		{"microsoft online services team", "microsoftonline.com", false},
		{"quickbooks", "fakebooks.example", true},
		{"netflix member", "gentleguideline.uk", true},
		{"lowe’s surprise gift", "libertysafe.com", true},
		{"example security team", "buyinfla.com", true},
		{"no-reply@ssa.gov", "mfr-system36.com", true},
		{"jane roe", "newco.example", false},
	} {
		if got := g.impersonation(c.name, c.domain) != ""; got != c.flagged {
			t.Errorf("impersonation(%q, %s) = %v", c.name, c.domain, got)
		}
	}
}
