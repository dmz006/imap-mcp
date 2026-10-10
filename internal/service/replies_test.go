package service

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/intel"
	"github.com/dmz006/imap-mcp/internal/testutil/imaptest"
)

// TestReplyLists covers Q1 (D32, D33): what needs a reply, what is awaited,
// and the ways an item clears — a sent reply, \Answered on the server, a
// dismiss — plus the digest's "Waiting on you" section.
func TestReplyLists(t *testing.T) {
	f := newHoldFixture(t)
	ctx := context.Background()
	me := imaptest.Username
	now := time.Now()
	ago := func(days int) int64 { return now.Add(-time.Duration(days)*24*time.Hour - time.Hour).Unix() }
	f.srv.Append(t, "INBOX", raw("t1@x", "Friend <friend@example.org>", me, "Dinner?"), now)                                          // UID 1
	f.srv.Append(t, "INBOX", raw("t5@x", "Friend <friend@example.org>", me, "Answered"), now, imaplib.FlagAnswered)                   // UID 2
	f.srv.Append(t, "INBOX", raw("f1@x", "Jane Roe <jane@newco.example>", me, "Hello from Jane"), now)                                // UID 3: clean first contact
	f.srv.Append(t, "INBOX", raw("f2@x", "Deals <deals@promo.example>", me, "Hi", "List-Unsubscribe: <mailto:u@promo.example>"), now) // UID 4: bulk

	st := f.d.StateSQL()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := st.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO senders(address, name, domain, role, sent_count) VALUES('friend@example.org','Friend','example.org','personal',2),
		('news@list.example','News','list.example','newsletter',0), ('who@unknown.example','','unknown.example','unknown',0),
		('jane@newco.example','Jane Roe','newco.example','personal',0), ('deals@promo.example','Deals','promo.example','personal',0)`)
	thread := func(id string, outgoing int, counterpart string, days int, uid int) {
		exec(`INSERT INTO reply_threads(account, thread_hash, thread_id, last_hash, last_date, outgoing, direct, counterpart, subject, message_ref, folder, uid)
			VALUES('test',?,?,?,?,?,?,?,?,?,'INBOX',?)`, intel.MsgHash(id), id, intel.MsgHash(id), ago(days), outgoing, 1-outgoing, counterpart, "s-"+id, id, uid)
	}
	thread("t1@x", 0, "friend@example.org", 5, 1)  // needs a reply
	thread("t2@x", 0, "news@list.example", 5, 0)   // newsletter: never
	thread("t3@x", 0, "who@unknown.example", 5, 0) // unknown sender, never written to: no
	thread("t4@x", 0, "friend@example.org", 5, 0)  // the owner replied (index)
	thread("t5@x", 0, "friend@example.org", 5, 2)  // \Answered on the server
	thread("t6@x", 0, "friend@example.org", 1, 0)  // too recent
	thread("t7@x", 0, "friend@example.org", 5, 0)  // held by a new_sender rule
	thread("f1@x", 0, "jane@newco.example", 5, 3)  // first contact, clean headers, model says conversation: listed
	thread("f2@x", 0, "deals@promo.example", 5, 4) // first contact with bulk headers: no
	thread("tr@x", 0, "friend@example.org", 5, 0)  // in Trash: never needs a reply (D48)
	exec(`UPDATE reply_threads SET folder = 'Trash' WHERE message_ref = 'tr@x'`)
	for _, id := range []string{"f1@x", "f2@x"} {
		if _, err := f.d.SQL().Exec(`INSERT INTO messages(account, folder, uid, from_addr, date, message_id, hall) VALUES('test','INBOX',?,'x',1,?,'conversation')`,
			len(id)*100+int(id[1]), id); err != nil {
			t.Fatal(err)
		}
	}
	thread("o1@x", 1, "friend@example.org", 4, 0)   // awaiting their reply
	thread("o2@x", 1, "friend@example.org", 120, 0) // older than the 90-day window
	exec(`INSERT INTO intel_messages(account, msg_hash, date, outgoing, reply_hash) VALUES('test', 99, ?, 1, ?)`, now.Unix(), intel.MsgHash("t4@x"))
	exec(`INSERT INTO held_messages(account, msg_hash, held_at) VALUES('test', ?, ?)`, intel.MsgHash("t7@x"), now.Unix())
	exec(`INSERT INTO intel_scan(account, folder, last_uid, completed_at, rescan_until) VALUES('test','INBOX',10,1,20)`)

	// D49: correspondents only until Q3; the vetted first-contact path is
	// still checked with the switch on.
	if n, err := f.s.NeedsReply(ctx, ReplyParams{OlderThanDays: 2}); err != nil || n.Count != 1 || n.Items[0].MessageRef != "t1@x" {
		t.Fatalf("needs_reply with first contacts off = %+v, %v", n, err)
	}
	replyFirstContacts = true
	defer func() { replyFirstContacts = false }()
	needs, err := f.s.NeedsReply(ctx, ReplyParams{OlderThanDays: 2})
	if err != nil {
		t.Fatal(err)
	}
	refs := []string{}
	for _, it := range needs.Items {
		refs = append(refs, it.MessageRef)
	}
	sort.Strings(refs)
	if strings.Join(refs, " ") != "f1@x t1@x" || needs.HistoryComplete {
		t.Fatalf("needs_reply = %v (history %v), want f1@x t1@x", refs, needs.HistoryComplete)
	}
	for _, it := range needs.Items {
		if it.MessageRef == "t1@x" && (it.ThreadID != "t1@x" || it.DaysWaiting != 5 || it.Name != "Friend") {
			t.Errorf("t1 item = %+v", it)
		}
	}
	var answered int
	st.QueryRow(`SELECT answered FROM reply_threads WHERE message_ref = 't5@x'`).Scan(&answered) //nolint:errcheck
	if answered != 1 {
		t.Error("\\Answered found on the server was not recorded")
	}
	awaiting, err := f.s.AwaitingReply(ctx, ReplyParams{OlderThanDays: 2})
	if err != nil || awaiting.Count != 1 || awaiting.Items[0].MessageRef != "o1@x" {
		t.Fatalf("awaiting_reply = %+v, %v", awaiting, err)
	}
	if all, _ := f.s.AwaitingReply(ctx, ReplyParams{WithinDays: 3650}); all.Count != 2 {
		t.Errorf("awaiting over all history = %d, want 2", all.Count)
	}
	if _, err := f.s.NeedsReply(ctx, ReplyParams{OlderThanDays: -1}); err == nil {
		t.Error("negative older_than_days accepted")
	}

	// The digest lists it under "Waiting on you", with no held mail.
	f.s.cfg.Rules.HoldDigestHour = 0
	var events []bus.Event
	f.b.Subscribe(bus.EventHoldDigest, func(e bus.Event) { events = append(events, e) })
	if err := f.s.sendHoldDigests(now); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, "INBOX"); n != 5 || len(events) != 1 || events[0].Payload.(map[string]any)["waiting"] != 2 {
		t.Fatalf("digest: INBOX=%d events=%+v", n, events)
	}
	if err := f.s.sendHoldDigests(now.Add(time.Minute)); err != nil || f.count(t, "INBOX") != 5 {
		t.Errorf("second digest the same day: %v", err)
	}
	msg := string(digestMessage("user@example.com", "test", nil, needs, digestLearning{}, now))
	if !strings.Contains(msg, "Waiting on you: 2") || !strings.Contains(msg, "Friend <friend@example.org>") || !strings.Contains(msg, "still being scanned") {
		t.Errorf("digest body:\n%s", msg)
	}

	// Dismissed: gone until a newer message arrives in the conversation.
	if _, err := f.s.DismissReply(ctx, "test", "t1@x"); err != nil {
		t.Fatal(err)
	}
	if n, _ := f.s.NeedsReply(ctx, ReplyParams{OlderThanDays: 2}); n.Count != 1 {
		t.Errorf("dismissed item still listed: %+v", n)
	}
	exec(`UPDATE reply_threads SET last_hash = 12345 WHERE message_ref = 't1@x'`)
	if n, _ := f.s.NeedsReply(ctx, ReplyParams{OlderThanDays: 2}); n.Count != 2 {
		t.Errorf("a newer message did not re-open the conversation: %+v", n)
	}
	if _, err := f.s.DismissReply(ctx, "test", "unknown@x"); err == nil {
		t.Error("dismissing an unknown thread succeeded")
	}
	exec(`UPDATE intel_scan SET last_uid = 20`)
	if n, _ := f.s.NeedsReply(ctx, ReplyParams{OlderThanDays: 2}); !n.HistoryComplete {
		t.Error("history_complete false after the rescan caught up")
	}
}

// TestRescanProgress (D45): health shows the reply-tracking rescan per
// account, and reply_history_complete once every account has caught up.
func TestRescanProgress(t *testing.T) {
	f := newHoldFixture(t)
	if _, err := f.d.StateSQL().Exec(`INSERT INTO intel_scan(account, folder, last_uid, completed_at, rescan_until)
		VALUES('test','INBOX',10,1,20), ('test','Sent',5,1,NULL)`); err != nil {
		t.Fatal(err)
	}
	st, err := f.s.IntelStats(context.Background())
	if err != nil || st.ReplyHistoryComplete || st.Accounts[0].RescanComplete || st.Accounts[0].RescanFoldersRemaining != 1 {
		t.Fatalf("during rescan = %+v, %v", st, err)
	}
	if _, err := f.d.StateSQL().Exec(`UPDATE intel_scan SET last_uid = 20 WHERE folder = 'INBOX'`); err != nil {
		t.Fatal(err)
	}
	if st, _ := f.s.IntelStats(context.Background()); !st.ReplyHistoryComplete || !st.Accounts[0].RescanComplete {
		t.Errorf("after rescan = %+v", st.Accounts)
	}
}
