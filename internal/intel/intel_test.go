package intel

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/testutil/imaptest"
)

func TestParseFields(t *testing.T) {
	var h Header
	parseFields(&h, []byte("List-Unsubscribe: <mailto:u@example.com>\r\nPrecedence: Bulk\r\nAuto-Submitted: auto-generated\r\n"+
		"Authentication-Results: mx.example.com; dkim=pass header.d=example.com; dmarc=fail\r\n"+
		"Authentication-Results: forged.example; dkim=fail; dmarc=pass\r\n\r\n"))
	if !h.List || !h.Bulk || !h.Auto || h.DKIM != "pass" || h.DMARC != "fail" {
		t.Fatalf("parsed = %+v", h)
	}
	h = Header{}
	parseFields(&h, []byte("Auto-Submitted: no\r\nPrecedence: first-class\r\nAuthentication-Results: mx; dkim=none\r\n\r\n"))
	if h.List || h.Bulk || h.Auto || h.DKIM != "fail" || h.DMARC != "" {
		t.Fatalf("parsed = %+v", h)
	}
}

func TestMsgHash(t *testing.T) {
	if msgHash("<A1@Example.com>") != msgHash(" a1@example.com ") || msgHash("a1@example.com") == msgHash("a2@example.com") {
		t.Error("hash must normalise brackets, case and space, and differ per id")
	}
	if msgHash("") != 0 || msgHash("<>") != 0 {
		t.Error("empty id must hash to 0")
	}
}

func TestSignalAndHallRoles(t *testing.T) {
	own := map[string]bool{"example.com": true, "gmail.com": true}
	cases := []struct {
		s    senderStats
		want string
	}{
		{senderStats{Address: "news@example.net", Received: 4, List: 3}, RoleNewsletter},
		{senderStats{Address: "x@example.net", Received: 4, Bulk: 1}, RoleUnknown}, // minority bulk
		{senderStats{Address: "alerts@example.net", Received: 2, Auto: 2}, RoleBot},
		{senderStats{Address: "no-reply@shop.example.net", Received: 1}, RoleBot},
		{senderStats{Address: "noreply+x@example.org", Received: 1}, RoleBot},
		{senderStats{Address: "alice@example.com", Domain: "example.com", Received: 3, Sent: 1}, RoleColleague},
		{senderStats{Address: "bob@gmail.com", Domain: "gmail.com", Sent: 2}, RolePersonal}, // webmail is never "colleague"
		{senderStats{Address: "carol@other.org", Domain: "other.org", Received: 1, Sent: 1}, RolePersonal},
		{senderStats{Address: "dave@other.org", Domain: "other.org", Received: 1}, RoleUnknown},
	}
	for _, c := range cases {
		if got, _ := signalRole(c.s, own); got != c.want {
			t.Errorf("%s: %s, want %s", c.s.Address, got, c.want)
		}
	}
	for hall, want := range map[string]string{"newsletter": RoleNewsletter, "transactional": RoleVendor, "alert": RoleBot,
		"conversation": RolePersonal, "unclassified": RoleUnknown, "": RoleUnknown} {
		if got := hallRole(hall); got != want {
			t.Errorf("hallRole(%q) = %s", hall, got)
		}
	}
}

func TestParseRoleAndPrompt(t *testing.T) {
	for in, want := range map[string]string{
		`{"role":"vendor"}`: RoleVendor,
		"<think>maybe {\"role\":\"bot\"}</think>\n{\"role\": \"Personal\"}": RolePersonal,
		`{"role":"overlord"}`: RoleUnknown,
		`no json`:             RoleUnknown,
	} {
		if got := parseRole(in); got != want {
			t.Errorf("parseRole(%q) = %s", in, got)
		}
	}
	p := rolePrompt("a@example.com", "Ann", []string{"Invoice 42", strings.Repeat("é", 150)})
	if !strings.Contains(p, "<a@example.com>") || !strings.Contains(p, "- Invoice 42") || !strings.Contains(p, `"role"`) {
		t.Errorf("prompt = %q", p)
	}
}

func TestScope(t *testing.T) {
	sc := &Scanner{cfg: config.IntelConfig{ExcludeFolders: []string{"archive/old"}}}
	gmail := []Folder{{Name: "INBOX"}, {Name: "[Gmail]", Attrs: []string{`\Noselect`}}, {Name: "[Gmail]/All Mail", Attrs: []string{`\All`}},
		{Name: "[Gmail]/Spam", Attrs: []string{`\Junk`}}}
	if got := sc.scope(gmail); len(got) != 1 || got[0] != "[Gmail]/All Mail" {
		t.Errorf("gmail scope = %v", got)
	}
	other := []Folder{{Name: "INBOX"}, {Name: "Sent", Attrs: []string{`\Sent`}}, {Name: "Junk", Attrs: []string{`\Junk`}},
		{Name: "Drafts", Attrs: []string{`\Drafts`}}, {Name: "Archive/Old"}, {Name: "Trash", Attrs: []string{`\Trash`}}}
	if got := strings.Join(sc.scope(other), ","); got != "INBOX,Sent,Trash" {
		t.Errorf("scope = %s", got)
	}
}

// fixture: a test server, an imap.db + cache.db, and a scanner over them.
type fixture struct {
	srv   *imaptest.Server
	d     *db.DB
	sc    *Scanner
	calls []string // prompts sent to the model
	slept []time.Duration
}

func msg(id, from, to, extra string) string {
	return "From: " + from + "\r\nTo: " + to + "\r\nSubject: s-" + id + "\r\nMessage-ID: <" + id + ">\r\n" + extra +
		"Content-Type: text/plain\r\n\r\nbody of " + id + "\r\n"
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{}
	f.srv = imaptest.Start(t, []string{"Sent", "Archive", "Junk", "Drafts"})
	me := imaptest.Username // user@example.com
	t0 := time.Now().Add(-10 * 24 * time.Hour)
	ap := func(folder, raw string, at time.Time, flags ...imaplib.Flag) {
		f.srv.Append(t, folder, raw, at, flags...)
	}
	ap("INBOX", msg("a1@example.com", "Alice <alice@example.com>", me, "Authentication-Results: mx.example.com; dkim=pass; dmarc=pass\r\n"), t0)
	// The same message again in a second folder: a copy carries the same headers.
	ap("Archive", msg("a1@example.com", "Alice <alice@example.com>", me, "Authentication-Results: mx.example.com; dkim=pass; dmarc=pass\r\n"), t0)
	ap("Sent", msg("r1@example.com", me, "alice@example.com", "In-Reply-To: <a1@example.com>\r\n"), t0.Add(2*time.Hour), imaplib.FlagSeen)
	for i := 0; i < 3; i++ {
		ap("INBOX", msg("n"+strconv.Itoa(i)+"@list.example.net", "news@list.example.net", me, "List-Unsubscribe: <mailto:u@list.example.net>\r\n"), t0.Add(time.Duration(i)*time.Hour))
	}
	ap("INBOX", msg("o1@shop.example.org", "noreply@shop.example.org", me, ""), t0)
	ap("INBOX", msg("v1@vendor.example.org", "billing@vendor.example.org", me, "Authentication-Results: mx; dkim=fail; dmarc=fail\r\n"), t0)
	ap("INBOX", msg("m1@mystery.example", "who@mystery.example", me, ""), t0)
	ap("INBOX", msg("m2@puzzle.example", "what@puzzle.example", me, ""), t0)
	ap("Junk", msg("j1@spam.example", "spam@spam.example", me, ""), t0)
	ap("Drafts", msg("d1@example.com", me, "eve@example.com", ""), t0)

	dir := t.TempDir()
	d, err := db.Open(db.Options{Path: dir + "/imap.db"}, db.Options{Path: dir + "/cache.db"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	f.d = d
	// Cached, classified mail for the hall and model passes.
	for _, c := range []struct{ from, subject, hall string }{
		{"billing@vendor.example.org", "Your invoice", "transactional"},
		{"billing@vendor.example.org", "Receipt", "transactional"},
		{"who@mystery.example", "Quarterly sync", "unclassified"},
		{"what@puzzle.example", "Hello", "unclassified"},
	} {
		res, err := d.Messages.Insert(context.Background(), &db.CachedMessage{Account: "test", Folder: "INBOX", UID: uint32(len(c.subject) + len(c.from)),
			MessageID: c.from + c.subject, FromAddr: c.from, Subject: c.subject, Date: t0, InternalDate: t0, Flags: []string{}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.SQL().Exec(`UPDATE messages SET hall=? WHERE id=?`, c.hall, res.ID); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.Config{Accounts: []config.AccountConfig{f.srv.Account("test")}, Intel: config.IntelConfig{
		ScanIntervalMinutes: 15, BackfillPerMinute: 600, BatchSize: 2, LLMRolesPerTick: 5,
		ExcludeFolders: []string{"Junk", "Drafts"}, // the test server has no SPECIAL-USE attributes
	}}
	classify := func(ctx context.Context, prompt string) (string, error) {
		f.calls = append(f.calls, prompt)
		if strings.Contains(prompt, "puzzle.example") {
			return "", ErrGated
		}
		return `{"role":"vendor"}`, nil
	}
	f.sc = New(cfg, NewIMAPSource(imaptest.Pool(t, cfg, nil)), d.StateSQL(), d.SQL(), classify,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	f.sc.sleep = func(_ context.Context, dur time.Duration) { f.slept = append(f.slept, dur) }
	return f
}

type senderRow struct {
	Role, Source                                              string
	Received, Sent, List, Auto, DKIMp, DKIMf, DMARCf, Replies int
	Avg                                                       sql.NullInt64
}

func (f *fixture) sender(t *testing.T, addr string) senderRow {
	t.Helper()
	var r senderRow
	err := f.d.StateSQL().QueryRow(`SELECT role, COALESCE(role_source,''), message_count, sent_count, list_count, auto_count,
		dkim_pass, dkim_fail, dmarc_fail, reply_count, avg_reply_time FROM senders WHERE address=?`, addr).
		Scan(&r.Role, &r.Source, &r.Received, &r.Sent, &r.List, &r.Auto, &r.DKIMp, &r.DKIMf, &r.DMARCf, &r.Replies, &r.Avg)
	if err != nil {
		t.Fatalf("sender %s: %v", addr, err)
	}
	return r
}

func unseen(t *testing.T, srv *imaptest.Server) int {
	t.Helper()
	c, err := imapclient.DialInsecure(srv.Addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Login(imaptest.Username, imaptest.Password).Wait(); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, folder := range []string{"INBOX", "Sent", "Archive"} {
		if _, err := c.Select(folder, nil).Wait(); err != nil {
			t.Fatal(err)
		}
		sd, err := c.UIDSearch(&imaplib.SearchCriteria{NotFlag: []imaplib.Flag{imaplib.FlagSeen}}, nil).Wait()
		if err != nil {
			t.Fatal(err)
		}
		n += len(sd.AllUIDs())
	}
	return n
}

func TestScannerEndToEnd(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	before := unseen(t, f.srv)
	if err := f.sc.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if after := unseen(t, f.srv); after != before {
		t.Errorf("scan marked mail seen: %d → %d", before, after)
	}

	alice := f.sender(t, "alice@example.com")
	if alice.Received != 1 || alice.Sent != 1 || alice.Role != RoleColleague || alice.DKIMp != 1 {
		t.Errorf("alice = %+v (the Archive copy must not count twice)", alice)
	}
	if alice.Replies != 1 || !alice.Avg.Valid || alice.Avg.Int64 != 7200 {
		t.Errorf("alice reply stats = %+v", alice)
	}
	if n := f.sender(t, "news@list.example.net"); n.Received != 3 || n.List != 3 || n.Role != RoleNewsletter {
		t.Errorf("news = %+v", n)
	}
	if b := f.sender(t, "noreply@shop.example.org"); b.Role != RoleBot || b.Source != "signal:noreply" {
		t.Errorf("noreply = %+v", b)
	}
	if v := f.sender(t, "billing@vendor.example.org"); v.Role != RoleVendor || v.Source != "hall" || v.DKIMf != 1 || v.DMARCf != 1 {
		t.Errorf("vendor = %+v", v)
	}
	if m := f.sender(t, "who@mystery.example"); m.Role != RoleVendor || m.Source != "llm" {
		t.Errorf("model role = %+v", m)
	}
	if m := f.sender(t, "what@puzzle.example"); m.Role != RoleUnknown || m.Source != "" {
		t.Errorf("gated sender must stay unknown and unchecked: %+v", m)
	}
	for _, p := range f.calls {
		if strings.Contains(p, "body of") {
			t.Errorf("a message body reached the model: %q", p)
		}
	}
	var junk, indexed int
	f.d.StateSQL().QueryRow(`SELECT count(*) FROM senders WHERE address IN ('spam@spam.example','eve@example.com')`).Scan(&junk) //nolint:errcheck
	f.d.StateSQL().QueryRow(`SELECT count(*) FROM intel_messages`).Scan(&indexed)                                                //nolint:errcheck
	if junk != 0 {
		t.Error("excluded folders were scanned")
	}
	if indexed != 9 { // 10 scanned messages, one duplicate
		t.Errorf("index rows = %d, want 9", indexed)
	}
	if len(f.slept) == 0 || f.slept[0] != 2*time.Minute/600 {
		t.Errorf("rate limit sleeps = %v", f.slept)
	}

	// Second tick: incremental. A model role survives recomputation.
	calls := len(f.calls)
	f.srv.Append(t, "INBOX", msg("m3@mystery.example", "who@mystery.example", imaptest.Username, ""), time.Now())
	if err := f.sc.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if m := f.sender(t, "who@mystery.example"); m.Received != 2 || m.Role != RoleVendor {
		t.Errorf("after new mail = %+v", m)
	}
	if a := f.sender(t, "alice@example.com"); a.Received != 1 || a.Replies != 1 {
		t.Errorf("second tick recounted: %+v", a)
	}
	if len(f.calls) != calls+1 { // only the still-unknown, gated sender is retried
		t.Errorf("model calls on second tick = %d", len(f.calls)-calls)
	}
}

// TestUIDValidityRescan: a recreated folder gets a new UIDVALIDITY; the folder
// is rescanned and the D28 index keeps every message counted once.
func TestUIDValidityRescan(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if err := f.sc.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.User.Delete("Archive"); err != nil {
		t.Fatal(err)
	}
	if err := f.srv.User.Create("Archive", nil); err != nil {
		t.Fatal(err)
	}
	f.srv.Append(t, "Archive", msg("a1@example.com", "Alice <alice@example.com>", imaptest.Username, ""), time.Now())
	if err := f.sc.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if a := f.sender(t, "alice@example.com"); a.Received != 1 {
		t.Errorf("rescan double-counted: %+v", a)
	}
	var validity uint32
	f.d.StateSQL().QueryRow(`SELECT uidvalidity FROM intel_scan WHERE folder='Archive'`).Scan(&validity) //nolint:errcheck
	if validity == 0 {
		t.Error("new UIDVALIDITY not stored")
	}
}

func TestRunDisabled(t *testing.T) {
	off := false
	sc := New(&config.Config{Intel: config.IntelConfig{Enabled: &off}}, nil, nil, nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	done := make(chan struct{})
	go func() { sc.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("disabled scanner must return immediately")
	}
}
