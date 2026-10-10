package service

import (
	"context"
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
	f.srv.Append(t, "INBOX", raw("t1@x", "Friend <friend@example.org>", me, "Dinner?"), now)                        // UID 1
	f.srv.Append(t, "INBOX", raw("t5@x", "Friend <friend@example.org>", me, "Answered"), now, imaplib.FlagAnswered) // UID 2

	st := f.d.StateSQL()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := st.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO senders(address, name, domain, role) VALUES('friend@example.org','Friend','example.org','personal'),
		('news@list.example','News','list.example','newsletter'), ('who@unknown.example','','unknown.example','unknown')`)
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
	thread("o1@x", 1, "friend@example.org", 4, 0)  // awaiting their reply
	thread("o2@x", 1, "friend@example.org", 40, 0) // older than the window
	exec(`INSERT INTO intel_messages(account, msg_hash, date, outgoing, reply_hash) VALUES('test', 99, ?, 1, ?)`, now.Unix(), intel.MsgHash("t4@x"))
	exec(`INSERT INTO held_messages(account, msg_hash, held_at) VALUES('test', ?, ?)`, intel.MsgHash("t7@x"), now.Unix())
	exec(`INSERT INTO intel_scan(account, folder, last_uid, completed_at, rescan_until) VALUES('test','INBOX',10,1,20)`)

	needs, err := f.s.NeedsReply(ctx, ReplyParams{OlderThanDays: 2})
	if err != nil {
		t.Fatal(err)
	}
	if needs.Count != 1 || needs.Items[0].MessageRef != "t1@x" || needs.Items[0].ThreadID != "t1@x" || needs.Items[0].DaysWaiting != 5 || needs.Items[0].Name != "Friend" || needs.HistoryComplete {
		t.Fatalf("needs_reply = %+v", needs)
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
	if n := f.count(t, "INBOX"); n != 3 || len(events) != 1 || events[0].Payload.(map[string]any)["waiting"] != 1 {
		t.Fatalf("digest: INBOX=%d events=%+v", n, events)
	}
	if err := f.s.sendHoldDigests(now.Add(time.Minute)); err != nil || f.count(t, "INBOX") != 3 {
		t.Errorf("second digest the same day: %v", err)
	}
	msg := string(digestMessage("user@example.com", "test", nil, needs, now))
	if !strings.Contains(msg, "Waiting on you: 1") || !strings.Contains(msg, "Friend <friend@example.org>") || !strings.Contains(msg, "still being scanned") {
		t.Errorf("digest body:\n%s", msg)
	}

	// Dismissed: gone until a newer message arrives in the conversation.
	if _, err := f.s.DismissReply(ctx, "test", needs.Items[0].ThreadID); err != nil {
		t.Fatal(err)
	}
	if n, _ := f.s.NeedsReply(ctx, ReplyParams{OlderThanDays: 2}); n.Count != 0 {
		t.Errorf("dismissed item still listed: %+v", n)
	}
	exec(`UPDATE reply_threads SET last_hash = 12345 WHERE message_ref = 't1@x'`)
	if n, _ := f.s.NeedsReply(ctx, ReplyParams{OlderThanDays: 2}); n.Count != 1 {
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
