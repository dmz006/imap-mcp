package service

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emersion/go-msgauth/dkim"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/testutil/imaptest"
	"github.com/dmz006/imap-mcp/internal/unsubscribe"
)

// TestUnsubscribe covers Q4 (D39–D41, D56, D57): dry run first, one-click
// with a DKIM-covered header, archive, mailto needing send, and senders that
// keep mailing after the grace period.
func TestUnsubscribe(t *testing.T) {
	var posts atomic.Int32
	web := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodPost && string(b) == "List-Unsubscribe=One-Click" {
			posts.Add(1)
		}
	}))
	defer web.Close()

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	oldOpt, oldClient := unsubOptions, unsubClient
	t.Cleanup(func() { unsubOptions, unsubClient = oldOpt, oldClient })
	unsubOptions = unsubscribe.Options{LookupTXT: func(domain string) ([]string, error) {
		if domain != "s1._domainkey.list.example.com" {
			return nil, &net.DNSError{Err: "no such host", Name: domain, IsNotFound: true}
		}
		return []string{"v=DKIM1; k=ed25519; p=" + base64.StdEncoding.EncodeToString(pub)}, nil
	}}
	unsubClient = func(*config.Config) *unsubscribe.Client {
		c := unsubscribe.NewClient("test", 2*time.Second, true)
		c.TrustTLS(web.Client().Transport.(*http.Transport).TLSClientConfig)
		return c
	}
	sign := func(msg string) string {
		var out bytes.Buffer
		if err := dkim.Sign(&out, strings.NewReader(msg), &dkim.SignOptions{Domain: "list.example.com", Selector: "s1", Signer: priv,
			HeaderKeys: []string{"From", "Subject", "List-Unsubscribe", "List-Unsubscribe-Post"}}); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}

	srv := imaptest.Start(t, []string{"Archive"})
	now := time.Now()
	for i, id := range []string{"n1@list.example.com", "n2@list.example.com", "n3@list.example.com"} {
		srv.Append(t, "INBOX", sign("From: News <news@list.example.com>\r\nTo: "+imaptest.Username+"\r\nSubject: Weekly\r\nMessage-ID: <"+id+
			">\r\nList-Unsubscribe: <"+web.URL+"/u/token>\r\nList-Unsubscribe-Post: List-Unsubscribe=One-Click\r\n\r\nNews.\r\n"), now.Add(time.Duration(i)*time.Minute))
	}
	srv.Append(t, "INBOX", "From: Old <old@mailto.example.com>\r\nTo: "+imaptest.Username+"\r\nSubject: Promo\r\nMessage-ID: <m1@mailto.example.com>\r\n"+
		"List-Unsubscribe: <mailto:leave@mailto.example.com?subject=remove>\r\n\r\nPromo.\r\n", now)
	srv.Append(t, "INBOX", imaptest.Plain("p1@example.org", "pal@example.org", "hi", "hello"), now)

	cfg := &config.Config{Accounts: []config.AccountConfig{srv.Account("test")}}
	b := bus.New()
	var events atomic.Int32
	b.Subscribe(bus.EventUnsubscribeDone, func(bus.Event) { events.Add(1) })
	pool := imaptest.Pool(t, cfg, b)
	dir := t.TempDir()
	d, err := db.Open(db.Options{Path: filepath.Join(dir, "imap.db")}, db.Options{Path: filepath.Join(dir, "cache.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	s := New(cfg, pool, d, nil, nil)
	ctx := context.Background()

	// A confirmed call needs a dry run first (D39).
	if _, err := s.Unsubscribe(ctx, UnsubscribeParams{Sender: "news@list.example.com", Confirm: true}); KindOf(err) != KindInvalid {
		t.Fatalf("confirm without dry run: %v", err)
	}
	res, err := s.Unsubscribe(ctx, UnsubscribeParams{Sender: "News <news@list.example.com>"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "dry_run" || res.Plan.Method != unsubscribe.MethodOneClick || res.Messages != 3 || res.Plan.SignedBy != "list.example.com" || posts.Load() != 0 {
		t.Fatalf("dry run = %+v, posts %d", res, posts.Load())
	}
	res, err = s.Unsubscribe(ctx, UnsubscribeParams{Sender: "news@list.example.com", Confirm: true, ArchiveTo: "Archive"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "done" || res.HTTP == nil || res.HTTP.Status != 200 || res.Archived != 3 || posts.Load() != 1 || events.Load() != 1 {
		t.Fatalf("confirmed = %+v, posts %d, events %d", res, posts.Load(), events.Load())
	}
	page, err := s.ListMessages(ctx, ListMessagesParams{Folder: "Archive"})
	if err != nil || page.Total != 3 {
		t.Fatalf("archive holds %d (%v)", page.Total, err)
	}

	// mailto: shown in the dry run; sending it needs the send scope (D40).
	res, err = s.Unsubscribe(ctx, UnsubscribeParams{Sender: "old@mailto.example.com"})
	if err != nil || res.Plan.Method != unsubscribe.MethodMailto || res.Plan.Mailto.To != "leave@mailto.example.com" || res.Plan.Mailto.Subject != "remove" {
		t.Fatalf("mailto dry run = %+v (%v)", res, err)
	}
	if _, err := s.Unsubscribe(ctx, UnsubscribeParams{Sender: "old@mailto.example.com", Confirm: true}); KindOf(err) != KindForbidden {
		t.Fatalf("mailto without send: %v", err)
	}

	// No List-Unsubscribe: not possible, and nothing is sent.
	res, err = s.Unsubscribe(ctx, UnsubscribeParams{Sender: "pal@example.org"})
	if err != nil || res.Status != "not_possible" {
		t.Fatalf("plain sender = %+v (%v)", res, err)
	}

	// Mail after the grace period is reported once (D56).
	st := d.StateSQL()
	r, err := st.Exec(`INSERT INTO senders(address, domain) VALUES('news@list.example.com','list.example.com')`)
	if err != nil {
		t.Fatal(err)
	}
	sid, _ := r.LastInsertId()
	if _, err := st.Exec(`INSERT INTO intel_messages(account, msg_hash, date, sender_id) VALUES('test', 1, ?, ?), ('test', 2, ?, ?)`,
		now.Add(24*time.Hour).Unix(), sid, now.Add(4*24*time.Hour).Unix(), sid); err != nil {
		t.Fatal(err)
	}
	log, err := s.ListUnsubscribes(ctx, "", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	if log.Count != 1 || len(log.Ignored) != 1 || log.Ignored[0].Since != 1 || !strings.Contains(log.Ignored[0].Rule, `"news@list.example.com"`) {
		t.Fatalf("log = %+v", log)
	}
	for _, it := range log.Items {
		if strings.Contains(it.Host, "token") || strings.Contains(it.Error, "token") {
			t.Fatalf("the URL leaked into the log: %+v", it)
		}
	}
	if err := s.markIgnoredReported(ctx, log.Ignored); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.ignoredUnsubscribes(ctx, "test", true); len(again) != 0 {
		t.Fatalf("reported twice: %+v", again)
	}
	rep, err := s.SetupCheck(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range rep.Findings {
		found = found || f.ID == "unsubscribe_ignored"
	}
	if !found {
		t.Fatalf("setup_check has no unsubscribe_ignored finding: %+v", rep.Findings)
	}
}
