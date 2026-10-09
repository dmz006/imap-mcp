package inbound

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/testutil/imaptest"
)

// TestWatcherLeavesOrdinaryMailUnread: polling reads every unseen message,
// but only command attempts may be marked \Seen.
func TestWatcherLeavesOrdinaryMailUnread(t *testing.T) {
	srv := imaptest.Start(t, nil)
	now := time.Now()
	srv.Append(t, "INBOX", imaptest.Plain("m1@example.com", "friend@example.org", "Lunch?", "see you at noon"), now)
	srv.Append(t, "INBOX", imaptest.Plain("m2@example.com", "ops@example.com", "status", envelope("status", "n1", now)), now)

	acct := srv.Account("a")
	acct.Inbound = &config.InboundConfig{Enabled: true, Gates: config.GateConfig{Allowlist: []string{"example.com"}}, Capabilities: []string{"status"}}
	cfg := &config.Config{Accounts: []config.AccountConfig{acct}}
	proc, b := newProc()
	w := NewWatcher(cfg, imaptest.Pool(t, cfg, b), proc, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := w.pollAccount(context.Background(), &cfg.Accounts[0]); err != nil {
		t.Fatal(err)
	}

	c, err := imapclient.DialInsecure(srv.Addr, nil)
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
	sd, err := c.UIDSearch(&imaplib.SearchCriteria{NotFlag: []imaplib.Flag{imaplib.FlagSeen}}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	if got := sd.AllUIDs(); len(got) != 1 || got[0] != 1 {
		t.Errorf("unseen after poll = %v, want [1] (ordinary mail unread, command marked seen)", got)
	}
}
