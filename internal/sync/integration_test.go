package sync

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/testutil/imaptest"
)

const multipartMsg = "From: =?ISO-8859-1?Q?Jos=E9?= <jose@example.com>\r\n" +
	"To: user@example.com\r\n" +
	"Subject: =?UTF-8?Q?R=C3=A9sum=C3=A9?=\r\n" +
	"Message-ID: <reply-1@example.com>\r\n" +
	"In-Reply-To: <root-1@example.com>\r\n" +
	"References: <root-1@example.com> <mid-1@example.com>\r\n" +
	"Date: Wed, 07 Oct 2026 10:00:00 +0000\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=XX\r\n\r\n" +
	"--XX\r\n" +
	"Content-Type: multipart/alternative; boundary=YY\r\n\r\n" +
	"--YY\r\n" +
	"Content-Type: text/plain; charset=ISO-8859-1\r\n" +
	"Content-Transfer-Encoding: quoted-printable\r\n\r\n" +
	"Caf=E9 meeting at noon=\r\n tomorrow\r\n" +
	"--YY\r\n" +
	"Content-Type: text/html; charset=UTF-8\r\n\r\n" +
	"<p>Caf\xc3\xa9 meeting</p>\r\n" +
	"--YY--\r\n" +
	"--XX\r\n" +
	"Content-Type: application/pdf\r\n" +
	"Content-Disposition: attachment; filename=\"agenda.pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n\r\n" +
	"JVBERi0xLjQK\r\n" +
	"--XX--\r\n"

func plainMsg(id, subject string) string {
	return imaptest.Plain(id, "a@example.com", subject, "hello")
}

func TestSyncAgainstIMAPServer(t *testing.T) {
	srv := imaptest.Start(t, []string{"Sent Items"})
	addr := srv.Addr
	now := time.Now()
	srv.Append(t, "INBOX", multipartMsg, now.Add(-24*time.Hour))
	srv.Append(t, "INBOX", plainMsg("old@example.com", "old"), now.AddDate(0, 0, -60))
	srv.Append(t, "INBOX", plainMsg("seen@example.com", "seen"), now.Add(-time.Hour), imaplib.FlagSeen)
	srv.Append(t, "Sent Items", plainMsg("sent@example.com", "sent"), now.Add(-2*time.Hour))

	cfg := &config.Config{
		Accounts: []config.AccountConfig{srv.Account("test")},
		// imapmemserver cannot mark folders SPECIAL-USE, so this uses the literal
		// name; token resolution is covered by the fake-source tests and live runs.
		Sync: config.SyncConfig{WindowDays: 30, MaxMessageMB: 25, Folders: []string{"INBOX", "Sent Items"}},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	b := bus.New()
	pool := imaptest.Pool(t, cfg, b)
	ctx := context.Background()
	dir := t.TempDir()
	d, err := db.Open(db.Options{Path: filepath.Join(dir, "imap.db")}, db.Options{Path: filepath.Join(dir, "cache.db"), Key: "k"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	s := New(cfg, pool, d, b, log)
	if err := s.SyncAccount(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	for _, st := range s.Stats() {
		if st.Error != "" {
			t.Fatalf("%s: %s", st.Entry, st.Error)
		}
	}

	counts, _ := d.Messages.Counts(ctx)
	got := map[string]int64{}
	for _, c := range counts {
		got[c.Folder] = c.Count
	}
	if got["INBOX"] != 2 || got["Sent Items"] != 1 {
		t.Fatalf("counts = %v (want INBOX 2 in window, Sent Items 1)", got)
	}

	var subject, fromName, fromAddr, text, html, attach, thread string
	if err := d.SQL().QueryRow(`SELECT subject, from_name, from_addr, body_text, body_html, attachments, thread_id
		FROM messages WHERE message_id='reply-1@example.com'`).Scan(&subject, &fromName, &fromAddr, &text, &html, &attach, &thread); err != nil {
		t.Fatal(err)
	}
	if subject != "Résumé" || fromName != "José" || fromAddr != "jose@example.com" {
		t.Errorf("headers: subject=%q from=%q <%s>", subject, fromName, fromAddr)
	}
	if !strings.Contains(text, "Café meeting at noon tomorrow") {
		t.Errorf("text not decoded: %q", text)
	}
	if !strings.Contains(html, "Café meeting") {
		t.Errorf("html = %q", html)
	}
	if !strings.Contains(attach, `"name":"agenda.pdf"`) || !strings.Contains(attach, `"size":9`) {
		t.Errorf("attachments = %s", attach)
	}
	if thread != "root-1@example.com" {
		t.Errorf("thread_id = %q", thread)
	}

	// The server is untouched: the unread message is still unread.
	c, err := imapclient.DialInsecure(addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.Login(imaptest.Username, imaptest.Password).Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	unseen, err := c.UIDSearch(&imaplib.SearchCriteria{NotFlag: []imaplib.Flag{imaplib.FlagSeen}}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(unseen.AllUIDs()); n != 2 {
		t.Errorf("unseen on server = %d, want 2 (sync must not set \\Seen)", n)
	}

	// Mark the multipart message seen and expunge the "seen" one on the server.
	if err := c.Store(imaplib.UIDSetNum(1), &imaplib.StoreFlags{Op: imaplib.StoreFlagsAdd, Flags: []imaplib.Flag{imaplib.FlagFlagged}}, nil).Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Store(imaplib.UIDSetNum(3), &imaplib.StoreFlags{Op: imaplib.StoreFlagsAdd, Flags: []imaplib.Flag{imaplib.FlagDeleted}}, nil).Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Expunge().Close(); err != nil {
		t.Fatal(err)
	}

	if err := s.SyncAccount(ctx, "test"); err != nil {
		t.Fatal(err)
	}
	uids, _ := d.Messages.CachedUIDs(ctx, "test", "INBOX")
	if _, ok := uids[3]; ok || len(uids) != 1 {
		t.Errorf("expunged message still cached: %v", uids)
	}
	if !slices.Contains(uids[1], `\Flagged`) {
		t.Errorf("flag change not picked up: %v", uids[1])
	}
}
