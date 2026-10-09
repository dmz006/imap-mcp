package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
	imapsync "github.com/dmz006/imap-mcp/internal/sync"
	"github.com/dmz006/imap-mcp/internal/testutil/imaptest"
)

// reply returns a text/plain reply carrying In-Reply-To and References.
func reply(messageID, from, subject, inReplyTo string, refs ...string) string {
	r := "From: " + from + "\r\nTo: " + imaptest.Username + "\r\nSubject: " + subject +
		"\r\nMessage-ID: <" + messageID + ">\r\nIn-Reply-To: <" + inReplyTo + ">\r\n"
	if len(refs) > 0 {
		r += "References: <" + strings.Join(refs, "> <") + ">\r\n"
	}
	return r + "Content-Type: text/plain\r\n\r\nreply body\r\n"
}

// threadSvc builds a service over a server holding a three-message thread:
// root (INBOX), our reply (Sent), their reply (INBOX), plus one unrelated message.
func threadSvc(t *testing.T) (*Service, *imaptest.Server) {
	t.Helper()
	srv := imaptest.Start(t, []string{"Sent"})
	now := time.Now()
	srv.Append(t, "INBOX", imaptest.Plain("root@example.com", "alice@example.com", "Plan", "first"), now.Add(-3*time.Hour))
	srv.Append(t, "Sent", reply("r1@example.com", imaptest.Username, "Re: Plan", "root@example.com", "root@example.com"), now.Add(-2*time.Hour), imaplib.FlagSeen)
	srv.Append(t, "INBOX", reply("r2@example.com", "alice@example.com", "Re: Plan", "r1@example.com", "root@example.com", "r1@example.com"), now.Add(-time.Hour))
	srv.Append(t, "INBOX", imaptest.Plain("other@example.com", "bob@example.com", "Unrelated", "x"), now)
	cfg := &config.Config{Accounts: []config.AccountConfig{srv.Account("test")}}
	pool := imaptest.Pool(t, cfg, nil)
	d, err := db.Open(db.Options{Path: t.TempDir() + "/imap.db"}, db.Options{Path: t.TempDir() + "/cache.db"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return New(cfg, pool, d, nil, nil), srv
}

func cacheMsg(t *testing.T, s *Service, folder string, uid uint32, msgID, threadID string, when time.Time) {
	t.Helper()
	if _, err := s.db.Messages.Insert(context.Background(), &db.CachedMessage{
		Account: "test", Folder: folder, UID: uid, MessageID: msgID, ThreadID: threadID,
		Subject: "Re: Plan", FromAddr: "alice@example.com", Date: when, InternalDate: when, Flags: []string{},
	}); err != nil {
		t.Fatal(err)
	}
}

func unseenCount(t *testing.T, srv *imaptest.Server, folder string) int {
	t.Helper()
	sd, err := inspect(t, srv, folder).UIDSearch(&imaplib.SearchCriteria{NotFlag: []imaplib.Flag{imaplib.FlagSeen}}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	return len(sd.AllUIDs())
}

// TestListMessagesThreadIDMatchesSync: live listings derive thread_id exactly
// as the cache does, so it can be passed to get_thread.
func TestListMessagesThreadIDMatchesSync(t *testing.T) {
	s, _ := threadSvc(t)
	page, err := s.ListMessages(context.Background(), ListMessagesParams{Folder: "INBOX"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range page.Messages {
		got[m.MessageID] = m.ThreadID
	}
	want := map[string]string{
		"root@example.com":  "root@example.com",                                                         // own Message-ID
		"r2@example.com":    imapsync.ThreadID([]string{"root@example.com", "r1@example.com"}, nil, ""), // References root
		"other@example.com": "other@example.com",
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("thread_id of %s = %q, want %q", id, got[id], w)
		}
	}
}

func TestGetThreadFromCacheWhenComplete(t *testing.T) {
	s, _ := threadSvc(t)
	now := time.Now()
	cacheMsg(t, s, "INBOX", 1, "root@example.com", "root@example.com", now.Add(-3*time.Hour))
	cacheMsg(t, s, "Sent", 1, "r1@example.com", "root@example.com", now.Add(-2*time.Hour))
	res, err := s.GetThread(context.Background(), ThreadParams{ThreadID: "<root@example.com>"})
	if err != nil {
		t.Fatal(err)
	}
	if res.LiveSearch {
		t.Error("root is cached: expected no live search")
	}
	if res.Count != 2 || res.Messages[0].MessageID != "root@example.com" || res.Messages[1].Folder != "Sent" {
		t.Fatalf("cache thread = %+v", res.Messages)
	}
}

// TestGetThreadLiveFallback: the root is not cached (outside the window), so
// the live search fills the gap without duplicating cached messages.
func TestGetThreadLiveFallback(t *testing.T) {
	s, srv := threadSvc(t)
	cacheMsg(t, s, "INBOX", 3, "r2@example.com", "root@example.com", time.Now().Add(-time.Hour))
	before := unseenCount(t, srv, "INBOX")
	res, err := s.GetThread(context.Background(), ThreadParams{ThreadID: "root@example.com", Folders: []string{"INBOX", "Sent"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.LiveSearch {
		t.Error("root not cached: expected a live search")
	}
	ids := []string{}
	sources := map[string]string{}
	for _, m := range res.Messages {
		ids = append(ids, m.MessageID)
		sources[m.MessageID] = m.Source
	}
	if strings.Join(ids, ",") != "root@example.com,r1@example.com,r2@example.com" {
		t.Fatalf("thread order = %v", ids)
	}
	if sources["r2@example.com"] != "cache" || sources["root@example.com"] != "live" {
		t.Errorf("sources = %v", sources)
	}
	if after := unseenCount(t, srv, "INBOX"); after != before {
		t.Errorf("live thread search marked mail seen: %d → %d", before, after)
	}
}

func TestGetThreadNothingCachedDefaultFolders(t *testing.T) {
	s, _ := threadSvc(t)
	// No \All or \Sent special-use on the test server: default scope is INBOX.
	res, err := s.GetThread(context.Background(), ThreadParams{ThreadID: "root@example.com", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !res.LiveSearch || res.Count != 1 || !res.Truncated {
		t.Fatalf("got %+v", res)
	}
	if _, err := s.GetThread(context.Background(), ThreadParams{}); KindOf(err) != KindInvalid {
		t.Errorf("empty thread_id: %v", err)
	}
}

// multipart fixture: body text, a binary attachment, a CSV attachment and an
// inline image.
var pdfBytes = []byte("%PDF-1.4\x00\x01\x02binary\xff\xfe")

func multipartMsg() string {
	b64 := base64.StdEncoding.EncodeToString(pdfBytes)
	return "From: alice@example.com\r\nTo: " + imaptest.Username + "\r\nSubject: Files\r\nMessage-ID: <files@example.com>\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=XX\r\n\r\n" +
		"--XX\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nsee attached\r\n" +
		"--XX\r\nContent-Type: application/pdf; name=\"../../etc/report.pdf\"\r\nContent-Disposition: attachment; filename=\"../../etc/report.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\n" + b64 + "\r\n" +
		"--XX\r\nContent-Type: text/csv; charset=utf-8\r\nContent-Disposition: attachment; filename=\"data.csv\"\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\na,b\r\n1,caf=C3=A9\r\n" +
		"--XX\r\nContent-Type: image/png\r\nContent-Disposition: inline\r\nContent-Transfer-Encoding: base64\r\n\r\niVBORw0KGgo=\r\n" +
		"--XX--\r\n"
}

func TestAttachmentsListAndFetch(t *testing.T) {
	s, srv := newSvc(t)
	srv.Append(t, "INBOX", multipartMsg(), time.Now())
	ctx := context.Background()
	const uid = 4
	before := unseenCount(t, srv, "INBOX")

	list, err := s.ListAttachments(ctx, "", "INBOX", uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("attachments = %+v", list)
	}
	if list[0].Part != "2" || list[0].MIME != "application/pdf" || list[1].Filename != "data.csv" || list[2].MIME != "image/png" {
		t.Errorf("attachments = %+v", list)
	}

	pdf, err := s.FetchAttachment(ctx, "", "INBOX", uid, "2", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pdf.Data, pdfBytes) || pdf.IsText() {
		t.Errorf("pdf decoded wrong: %q", pdf.Data)
	}
	csv, err := s.FetchAttachment(ctx, "", "INBOX", uid, "3", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !csv.IsText() || !strings.Contains(string(csv.Data), "café") {
		t.Errorf("csv decoded wrong: %q", csv.Data)
	}

	if _, err := s.FetchAttachment(ctx, "", "INBOX", uid, "1", 0); KindOf(err) != KindNotFound {
		t.Errorf("body text part is not an attachment: %v", err)
	}
	if _, err := s.FetchAttachment(ctx, "", "INBOX", uid, "2", 4); KindOf(err) != KindUnprocessable {
		t.Errorf("over cap: %v", err)
	}
	for _, bad := range []string{"", "0", "a", "1..2", "-1"} {
		if _, err := s.FetchAttachment(ctx, "", "INBOX", uid, bad, 0); KindOf(err) != KindInvalid {
			t.Errorf("part %q: %v", bad, err)
		}
	}
	if after := unseenCount(t, srv, "INBOX"); after != before {
		t.Errorf("attachment reads marked mail seen: %d → %d", before, after)
	}
}

func TestSafeFilename(t *testing.T) {
	cases := map[string]string{
		"../../etc/report.pdf": "report.pdf",
		`..\..\win.ini`:        "win.ini",
		".bashrc":              "bashrc",
		"a\x00b\nc.txt":        "abc.txt",
		"weird*name?.txt":      "weird_name_.txt",
		"":                     "def",
		"..":                   "def",
		"/":                    "def",
		"résumé.pdf":           "résumé.pdf",
	}
	for in, want := range cases {
		if got := SafeFilename(in, "def"); got != want {
			t.Errorf("SafeFilename(%q) = %q, want %q", in, got, want)
		}
	}
	if got := SafeFilename(strings.Repeat("é", 100)+".pdf", "def"); len(got) > 120 || !strings.HasSuffix(got, ".pdf") {
		t.Errorf("long name = %q (%d bytes)", got, len(got))
	}
}

func TestExportSingleEML(t *testing.T) {
	s, srv := newSvc(t)
	before := unseenCount(t, srv, "INBOX")
	res, err := s.Export(context.Background(), ExportParams{Folder: "INBOX", UID: 1})
	if err != nil {
		t.Fatal(err)
	}
	want := imaptest.Plain("m1@example.com", "alice@example.com", "Invoice 1", "pay me")
	if res.Format != "eml" || res.Count != 1 || string(res.Data) != want {
		t.Fatalf("eml = %q (format %s)", res.Data, res.Format)
	}
	if after := unseenCount(t, srv, "INBOX"); after != before {
		t.Errorf("export marked mail seen: %d → %d", before, after)
	}
}

func TestExportBatches(t *testing.T) {
	s, srv := newSvc(t)
	srv.Append(t, "INBOX", "From: carol@example.com\r\nSubject: quoting\r\nMessage-ID: <q@example.com>\r\n\r\nline\r\nFrom the start\r\n>From quoted\r\n", time.Now())
	ctx := context.Background()

	res, err := s.Export(ctx, ExportParams{Folder: "INBOX", UIDs: []uint32{1, 4}})
	if err != nil {
		t.Fatal(err)
	}
	mbox := string(res.Data)
	if res.Format != "mbox" || res.Count != 2 || strings.Count(mbox, "\nFrom ") != 1 || !strings.HasPrefix(mbox, "From alice@example.com ") {
		t.Fatalf("mbox = %q", mbox)
	}
	if !strings.Contains(mbox, "\n>From the start\n") || !strings.Contains(mbox, "\n>>From quoted\n") || strings.Contains(mbox, "\r\n") {
		t.Errorf("mboxrd quoting/line endings wrong: %q", mbox)
	}

	byFrom, err := s.Export(ctx, ExportParams{From: "alice@example.com"})
	if err != nil || byFrom.Count != 2 {
		t.Fatalf("from export: %+v, %v", byFrom, err)
	}

	if _, err := s.Export(ctx, ExportParams{From: "alice@example.com", MaxMessages: 1}); KindOf(err) != KindUnprocessable {
		t.Errorf("message cap: %v", err)
	}
	if _, err := s.Export(ctx, ExportParams{Folder: "INBOX", UIDs: []uint32{1, 2}, MaxBytes: 10}); KindOf(err) != KindUnprocessable {
		t.Errorf("byte cap: %v", err)
	}
	if _, err := s.Export(ctx, ExportParams{Folder: "INBOX", UID: 1, From: "x"}); KindOf(err) != KindInvalid {
		t.Errorf("two selectors: %v", err)
	}
	if _, err := s.Export(ctx, ExportParams{UID: 1}); KindOf(err) != KindInvalid {
		t.Errorf("uid without folder: %v", err)
	}
	if _, err := s.Export(ctx, ExportParams{Folder: "INBOX", UIDs: []uint32{1, 99}}); KindOf(err) != KindNotFound {
		t.Errorf("missing uid: %v", err)
	}
}

func TestExportThread(t *testing.T) {
	s, _ := threadSvc(t)
	res, err := s.Export(context.Background(), ExportParams{ThreadID: "root@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	// Default live scope on the test server is INBOX: root + their reply.
	if res.Count != 2 || len(res.Messages) != 2 {
		t.Fatalf("thread export = %+v", res)
	}
}

func TestCrossAccountSearchCache(t *testing.T) {
	s, _ := newSvc(t)
	ctx := context.Background()
	now := time.Now()
	add := func(account string, uid uint32, from, subject, body string, age time.Duration) {
		if _, err := s.db.Messages.Insert(ctx, &db.CachedMessage{
			Account: account, Folder: "INBOX", UID: uid, MessageID: account + "-" + subject,
			FromAddr: from, Subject: subject, BodyText: body, Date: now.Add(-age), InternalDate: now.Add(-age), Flags: []string{},
		}); err != nil {
			t.Fatal(err)
		}
	}
	add("work", 1, "billing@example.com", "Invoice May", "amount due", 48*time.Hour)
	add("work", 2, "billing@example.com", "Invoice June", "amount due", time.Hour)
	add("home", 1, "shop@example.net", "Receipt", "your invoice is attached", 2*time.Hour)
	add("home", 2, "friend@example.org", "Dinner", "see you", time.Hour)

	res, err := s.CrossAccountSearch(ctx, CrossSearchParams{Text: "invoice"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != "cache" || res.Count != 3 {
		t.Fatalf("text search = %+v", res)
	}
	if res.Hits[0].Subject != "Invoice June" { // newest first across accounts
		t.Errorf("order = %+v", res.Hits)
	}

	res, _ = s.CrossAccountSearch(ctx, CrossSearchParams{Text: "invoice", Limit: 1})
	if res.Count != 2 { // one per account
		t.Errorf("per-account limit: %+v", res.Hits)
	}
	res, _ = s.CrossAccountSearch(ctx, CrossSearchParams{From: "EXAMPLE.NET"})
	if res.Count != 1 || res.Hits[0].Account != "home" {
		t.Errorf("from search: %+v", res.Hits)
	}
	res, _ = s.CrossAccountSearch(ctx, CrossSearchParams{Subject: "invoice", Since: now.Add(-24 * time.Hour).Format("2006-01-02")})
	if res.Count < 1 {
		t.Errorf("since: %+v", res.Hits)
	}
	// FTS syntax and LIKE wildcards in user input are literal.
	for _, q := range []string{`" OR *`, `NEAR(a b)`, `subject:x`} {
		if _, err := s.CrossAccountSearch(ctx, CrossSearchParams{Text: q}); err != nil {
			t.Errorf("text %q: %v", q, err)
		}
	}
	if res, _ := s.CrossAccountSearch(ctx, CrossSearchParams{Subject: "%"}); res.Count != 0 {
		t.Errorf("%% matched %d", res.Count)
	}
	if _, err := s.CrossAccountSearch(ctx, CrossSearchParams{}); KindOf(err) != KindInvalid {
		t.Errorf("no criteria: %v", err)
	}
	if _, err := s.CrossAccountSearch(ctx, CrossSearchParams{From: "x", Since: "May 1"}); KindOf(err) != KindInvalid {
		t.Errorf("bad date: %v", err)
	}
}

// TestCrossAccountSearchLive: two servers searched in parallel; one going
// away is reported as that account's error, not a failed search.
func TestCrossAccountSearchLive(t *testing.T) {
	a := imaptest.Start(t, nil)
	b := imaptest.Start(t, nil)
	c := imaptest.Start(t, nil)
	now := time.Now()
	a.Append(t, "INBOX", imaptest.Plain("a1@example.com", "billing@example.com", "Invoice A", "x"), now.Add(-time.Hour))
	b.Append(t, "INBOX", imaptest.Plain("b1@example.com", "billing@example.com", "Invoice B", "x"), now)
	b.Append(t, "INBOX", imaptest.Plain("b2@example.com", "friend@example.org", "Hi", "x"), now)
	acA, acB, acC := a.Account("a"), b.Account("b"), c.Account("c")
	acB.Default, acC.Default = false, false
	cfg := &config.Config{Accounts: []config.AccountConfig{acA, acB, acC}}
	s := New(cfg, imaptest.Pool(t, cfg, nil), nil, nil, nil)
	c.Close()

	res, err := s.CrossAccountSearch(context.Background(), CrossSearchParams{From: "billing", Live: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != "live" || res.Count != 2 || res.Hits[0].Account != "b" || res.TotalMatches["a"] != 1 {
		t.Fatalf("live = %+v", res)
	}
	if len(res.Errors) != 1 || res.Errors[0].Account != "c" {
		t.Errorf("errors = %+v", res.Errors)
	}
	if _, err := s.CrossAccountSearch(context.Background(), CrossSearchParams{From: "x"}); KindOf(err) != KindUnavailable {
		t.Errorf("cache search without a cache: %v", err)
	}
}
