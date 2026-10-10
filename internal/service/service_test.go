package service

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/testutil/imaptest"
)

func newSvc(t *testing.T, caps ...imaplib.Cap) (*Service, *imaptest.Server) {
	t.Helper()
	srv := imaptest.Start(t, []string{"Trash", "Archive"}, caps...)
	now := time.Now()
	srv.Append(t, "INBOX", imaptest.Plain("m1@example.com", "alice@example.com", "Invoice 1", "pay me"), now.Add(-3*time.Hour))
	srv.Append(t, "INBOX", imaptest.Plain("m2@example.com", "bob@example.com", "Hello", "hi there"), now.Add(-2*time.Hour), imaplib.FlagSeen)
	srv.Append(t, "INBOX", imaptest.Plain("m3@example.com", "alice@example.com", "Invoice 2", "pay again"), now.Add(-time.Hour))
	cfg := &config.Config{Accounts: []config.AccountConfig{srv.Account("test")}}
	pool := imaptest.Pool(t, cfg, nil)
	dir := t.TempDir()
	d, err := db.Open(db.Options{Path: filepath.Join(dir, "imap.db")}, db.Options{Path: filepath.Join(dir, "cache.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return New(cfg, pool, d, nil, nil), srv
}

// inspect opens an independent client to check server state.
func inspect(t *testing.T, srv *imaptest.Server, folder string) *imapclient.Client {
	t.Helper()
	c, err := imapclient.DialInsecure(srv.Addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.Login(imaptest.Username, imaptest.Password).Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Select(folder, nil).Wait(); err != nil {
		t.Fatal(err)
	}
	return c
}

func uidsIn(t *testing.T, srv *imaptest.Server, folder string) []imaplib.UID {
	t.Helper()
	sd, err := inspect(t, srv, folder).UIDSearch(&imaplib.SearchCriteria{}, nil).Wait()
	if err != nil {
		t.Fatal(err)
	}
	return sd.AllUIDs()
}

func TestReadOperations(t *testing.T) {
	s, _ := newSvc(t)
	ctx := context.Background()

	folders, err := s.ListFolders(ctx, "")
	if err != nil || len(folders) != 3 {
		t.Fatalf("folders = %v, %v", folders, err)
	}
	page, err := s.ListMessages(ctx, ListMessagesParams{Folder: "INBOX", Limit: 2})
	if err != nil || page.Total != 3 || page.Count != 2 || page.Messages[0].Subject != "Invoice 2" {
		t.Fatalf("page = %+v, %v", page, err)
	}
	page, _ = s.ListMessages(ctx, ListMessagesParams{Folder: "INBOX", Limit: 2, Offset: 2})
	if page.Count != 1 || page.Messages[0].Subject != "Invoice 1" {
		t.Fatalf("page 2 = %+v", page)
	}
	page, _ = s.ListMessages(ctx, ListMessagesParams{Folder: "INBOX", Ascending: true, Limit: 1})
	if page.Messages[0].Subject != "Invoice 1" {
		t.Fatalf("asc = %+v", page)
	}

	res, err := s.Search(ctx, SearchParams{From: "alice", Limit: 1})
	if err != nil || res.TotalMatches != 2 || res.Returned != 1 || res.Messages[0].Subject != "Invoice 2" {
		t.Fatalf("search = %+v, %v", res, err)
	}
	if res, _ := s.Search(ctx, SearchParams{Flags: "unseen"}); res.TotalMatches != 2 {
		t.Errorf("unseen = %d", res.TotalMatches)
	}
	if _, err := s.Search(ctx, SearchParams{Since: "yesterday"}); KindOf(err) != KindInvalid {
		t.Errorf("bad date: %v", err)
	}
	if _, err := s.Search(ctx, SearchParams{Flags: "bogus"}); KindOf(err) != KindInvalid {
		t.Errorf("bad flag: %v", err)
	}

	msg, err := s.GetMessage(ctx, "", "INBOX", 2)
	if err != nil || !strings.Contains(msg.BodyText, "hi there") || msg.Subject != "Hello" {
		t.Fatalf("get = %+v, %v", msg, err)
	}
	if _, err := s.GetMessage(ctx, "", "INBOX", 99); KindOf(err) != KindNotFound {
		t.Errorf("missing uid: %v", err)
	}
	if _, err := s.GetMessage(ctx, "", "", 1); KindOf(err) != KindInvalid {
		t.Errorf("missing folder: %v", err)
	}
	if _, err := s.ListMessages(ctx, ListMessagesParams{Account: "nope"}); KindOf(err) != KindNotFound {
		t.Errorf("unknown account: %v", err)
	}
	h, err := s.GetHeaders(ctx, "_default", "INBOX", 1)
	if err != nil || !strings.Contains(h.RawHeaders, "Message-ID: <m1@example.com>") {
		t.Fatalf("headers = %+v, %v", h, err)
	}
}

func TestWriteOperations(t *testing.T) {
	for _, tc := range []struct {
		name string
		caps []imaplib.Cap
	}{
		{"with MOVE+UIDPLUS", []imaplib.Cap{imaplib.CapMove, imaplib.CapUIDPlus}},
		{"bare IMAP4rev1", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, srv := newSvc(t, tc.caps...)
			ctx := context.Background()

			if err := s.SetFlags(ctx, "", "INBOX", 1, "flagged", ""); err != nil {
				t.Fatal(err)
			}
			sd, _ := inspect(t, srv, "INBOX").UIDSearch(&imaplib.SearchCriteria{Flag: []imaplib.Flag{imaplib.FlagFlagged}}, nil).Wait()
			if !slices.Equal(sd.AllUIDs(), []imaplib.UID{1}) {
				t.Fatalf("flagged = %v", sd.AllUIDs())
			}
			if err := s.SetFlags(ctx, "", "INBOX", 1, "", ""); KindOf(err) != KindInvalid {
				t.Errorf("no-op flags: %v", err)
			}

			if err := s.CopyMessage(ctx, "", "INBOX", 2, "Archive"); err != nil {
				t.Fatal(err)
			}
			if got := uidsIn(t, srv, "Archive"); len(got) != 1 {
				t.Fatalf("archive after copy = %v", got)
			}

			// Another message already marked \Deleted (e.g. by a mail client)
			// must survive a targeted permanent delete.
			c := inspect(t, srv, "INBOX")
			if err := c.Store(imaplib.UIDSetNum(3), &imaplib.StoreFlags{Op: imaplib.StoreFlagsAdd, Flags: []imaplib.Flag{imaplib.FlagDeleted}}, nil).Close(); err != nil {
				t.Fatal(err)
			}
			_, err := s.DeleteMessage(ctx, "", "INBOX", 1, true)
			if tc.caps != nil {
				if err != nil {
					t.Fatal(err)
				}
				if got := uidsIn(t, srv, "INBOX"); !slices.Equal(got, []imaplib.UID{2, 3}) {
					t.Fatalf("after UID EXPUNGE of 1: %v (uid 3 must survive)", got)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "refusing a folder-wide EXPUNGE") {
					t.Fatalf("without UIDPLUS and another \\Deleted message: %v", err)
				}
				if got := uidsIn(t, srv, "INBOX"); !slices.Equal(got, []imaplib.UID{1, 2, 3}) {
					t.Fatalf("nothing may be expunged: %v", got)
				}
				// Clear the foreign \Deleted, then the fallback EXPUNGE is safe.
				c.Store(imaplib.UIDSetNum(3), &imaplib.StoreFlags{Op: imaplib.StoreFlagsDel, Flags: []imaplib.Flag{imaplib.FlagDeleted}}, nil).Close()
				if _, err := s.DeleteMessage(ctx, "", "INBOX", 1, true); err != nil {
					t.Fatal(err)
				}
				if got := uidsIn(t, srv, "INBOX"); !slices.Equal(got, []imaplib.UID{2, 3}) {
					t.Fatalf("after fallback expunge: %v", got)
				}
			}

			res, err := s.DeleteMessage(ctx, "", "INBOX", 2, false)
			if tc.caps != nil {
				if err != nil || res.Trash != "Trash" {
					t.Fatalf("to trash: %+v %v", res, err)
				}
				if got := uidsIn(t, srv, "Trash"); len(got) != 1 {
					t.Fatalf("trash = %v", got)
				}
			} else {
				// No MOVE/UIDPLUS: COPY + targeted delete (safe: no other \Deleted mail now).
				if err != nil || res.Trash != "Trash" {
					t.Fatalf("no-move delete: %+v %v", res, err)
				}
				if got := uidsIn(t, srv, "INBOX"); !slices.Equal(got, []imaplib.UID{3}) {
					t.Fatalf("inbox after copy+delete = %v", got)
				}
			}

			if err := s.MoveMessage(ctx, "", "Archive", 1, "INBOX"); err != nil {
				t.Fatal(err)
			}
			if got := uidsIn(t, srv, "Archive"); len(got) != 0 {
				t.Fatalf("archive after move = %v", got)
			}
			if err := s.MoveMessage(ctx, "", "INBOX", 0, "x"); KindOf(err) != KindInvalid {
				t.Errorf("invalid move: %v", err)
			}
			if err := s.CreateFolder(ctx, "", "New"); err != nil {
				t.Fatal(err)
			}
			if err := s.DeleteFolder(ctx, "", "New"); err != nil {
				t.Fatal(err)
			}
			if err := s.CreateFolder(ctx, "", ""); KindOf(err) != KindInvalid {
				t.Errorf("empty folder: %v", err)
			}
		})
	}
}

func TestRules(t *testing.T) {
	s, srv := newSvc(t, imaplib.CapMove)
	ctx := context.Background()
	if _, err := s.CreateRule(ctx, &db.Rule{Name: "x"}); KindOf(err) != KindInvalid {
		t.Errorf("no action: %v", err)
	}
	if _, err := s.CreateRule(ctx, &db.Rule{Name: "x", Actions: []db.RuleAction{{Type: "move"}}, Conditions: db.RuleConditions{From: "a"}}); KindOf(err) != KindInvalid {
		t.Errorf("move without dest: %v", err)
	}
	if _, err := s.CreateRule(ctx, &db.Rule{Name: "x", Actions: []db.RuleAction{{Type: "trash"}}}); KindOf(err) != KindInvalid {
		t.Errorf("no condition: %v", err)
	}
	id, err := s.CreateRule(ctx, &db.Rule{Name: "invoices", Active: true, Actions: []db.RuleAction{{Type: "move", Dest: "Archive"}}, Conditions: db.RuleConditions{From: "alice"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateRule(ctx, &db.Rule{Name: "invoices", Actions: []db.RuleAction{{Type: "seen"}}, Conditions: db.RuleConditions{From: "z"}}); KindOf(err) != KindInvalid {
		t.Errorf("duplicate name: %v", err)
	}

	res, err := s.TestRule(ctx, id)
	if err != nil || res.Matched != 2 {
		t.Fatalf("test = %+v, %v", res, err)
	}
	if got := uidsIn(t, srv, "INBOX"); len(got) != 3 {
		t.Fatal("TestRule must not change the mailbox")
	}

	r, _ := s.GetRule(ctx, id)
	r.Conditions.Subject = "Invoice 2"
	r.Active = false
	if err := s.UpdateRule(ctx, r); err != nil {
		t.Fatal(err)
	}
	r2, _ := s.GetRule(ctx, id)
	if r2.Active || r2.Conditions.Subject != "Invoice 2" {
		t.Fatalf("update not stored: %+v", r2)
	}
	if err := s.UpdateRule(ctx, &db.Rule{ID: 999, Name: "n", Actions: []db.RuleAction{{Type: "seen"}}, Conditions: db.RuleConditions{From: "a"}}); KindOf(err) != KindNotFound {
		t.Errorf("update unknown: %v", err)
	}
	if results, _ := s.RunActiveRules(0, false); len(results) != 0 {
		t.Errorf("inactive rule ran: %+v", results)
	}
	results, err := s.RunActiveRules(id, false) // explicit id runs even if inactive
	if err != nil || len(results) != 1 || results[0].Matched != 1 {
		t.Fatalf("run = %+v, %v", results, err)
	}
	if got := uidsIn(t, srv, "Archive"); len(got) != 1 {
		t.Fatalf("archive = %v", got)
	}
	rules, _ := s.ListRules(ctx)
	if len(rules) != 1 || rules[0].RunCount != 1 {
		t.Fatalf("rules = %+v", rules)
	}
	if err := s.DeleteRule(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRule(ctx, id); KindOf(err) != KindNotFound {
		t.Errorf("get deleted: %v", err)
	}
	if err := s.DeleteRule(ctx, id); KindOf(err) != KindNotFound {
		t.Errorf("delete twice: %v", err)
	}
}

func TestSendAndStatsErrors(t *testing.T) {
	s, _ := newSvc(t)
	ctx := context.Background()
	if _, err := s.Send(ctx, SendParams{To: "a@example.com"}); KindOf(err) != KindInvalid {
		t.Errorf("missing fields: %v", err)
	}
	if _, err := s.Send(ctx, SendParams{Account: "nope", To: "a@example.com", Subject: "s", Body: "b"}); KindOf(err) != KindNotFound {
		t.Errorf("unknown account: %v", err)
	}
	if _, err := s.Send(ctx, SendParams{To: "a@example.com", Subject: "s", Body: "b"}); KindOf(err) != KindUnprocessable {
		t.Errorf("receive-only account: %v", err)
	}
	st, err := s.AccountStats(ctx, "")
	if err != nil || st.Account != "test" || !st.Connected {
		t.Fatalf("stats = %+v, %v", st, err)
	}
	if _, err := s.AccountStats(ctx, "nope"); KindOf(err) != KindNotFound {
		t.Errorf("stats unknown: %v", err)
	}
	if _, err := s.SyncAccount(ctx, ""); KindOf(err) != KindUnavailable {
		t.Errorf("sync without syncer: %v", err)
	}
	if _, err := s.EnrichmentStats(ctx); KindOf(err) != KindUnavailable {
		t.Errorf("enrichment without pipeline: %v", err)
	}
	if _, err := s.SweepCache(ctx, db.SweepFilter{}, true); KindOf(err) != KindUnavailable {
		t.Errorf("sweep without syncer: %v", err)
	}
	if (&Error{Kind: KindInvalid, Msg: "m"}).Error() != "m" || KindOf(context.Canceled) != KindInternal {
		t.Error("error helpers")
	}
}

func TestParseFlags(t *testing.T) {
	got := ParseFlags(`seen, \Flagged ,,answered`)
	want := []string{`\seen`, `\flagged`, `\answered`} // IMAP system flags are case-insensitive
	var lower []string
	for _, f := range got {
		lower = append(lower, strings.ToLower(string(f)))
	}
	if !slices.Equal(lower, want) {
		t.Fatalf("%v", got)
	}
}

func TestValidRecipient(t *testing.T) {
	for in, want := range map[string]bool{
		"a@example.com": true, "Ann <a@example.com>": true,
		"imap_mcp": false, "a@localhost": false, "": false, "a@": false,
	} {
		if validRecipient(in) != want {
			t.Errorf("validRecipient(%q) != %v", in, want)
		}
	}
}
