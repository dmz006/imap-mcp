package db

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	d, err := Open(Options{Path: filepath.Join(dir, "imap.db")}, Options{Path: filepath.Join(dir, "cache.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func seed(t *testing.T, d *DB, account, folder string, uid uint32, ageDays int, status string) int64 {
	t.Helper()
	res, err := d.Messages.Insert(context.Background(), &CachedMessage{Account: account, Folder: folder, UID: uid,
		FromAddr: "a@example.com", Subject: "s", BodyText: "word", InternalDate: time.Now().AddDate(0, 0, -ageDays)})
	if err != nil {
		t.Fatal(err)
	}
	if status != "" {
		mustExec(t, d.SQL(), `UPDATE messages SET enrichment_status=? WHERE id=?`, status, res.ID)
	}
	if err := d.Sync.Put(context.Background(), account, folder, FolderSyncState{UIDValidity: 1}); err != nil {
		t.Fatal(err)
	}
	return res.ID
}

func count(t *testing.T, d *DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := d.SQL().QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSweepDryRunAndFilters(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	seed(t, d, "a", "INBOX", 1, 1, "")
	seed(t, d, "a", "INBOX", 2, 50, "")
	seed(t, d, "a", "Sent", 3, 50, "error")
	seed(t, d, "b", "INBOX", 4, 1, "error")

	got, err := d.Messages.Sweep(ctx, SweepFilter{OlderThanDays: 30}, true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Count != 1 || got[1].Count != 1 {
		t.Fatalf("dry run counts = %+v", got)
	}
	if n := count(t, d, `SELECT count(*) FROM messages`); n != 4 {
		t.Fatalf("dry run deleted rows: %d left", n)
	}

	got, _ = d.Messages.Sweep(ctx, SweepFilter{ErrorsOnly: true, Account: "b"}, false, time.Now())
	if len(got) != 1 || got[0].Account != "b" {
		t.Fatalf("errors_only+account = %+v", got)
	}
	if n := count(t, d, `SELECT count(*) FROM messages WHERE account='b'`); n != 0 {
		t.Error("b not swept")
	}
	if n := count(t, d, `SELECT count(*) FROM sync_state WHERE account='b'`); n != 0 {
		t.Error("swept folder's sync state must reset so it is re-fetched")
	}
	if n := count(t, d, `SELECT count(*) FROM enrichment_queue`); n != 3 {
		t.Errorf("queue rows cascade with messages: %d", n)
	}

	got, _ = d.Messages.Sweep(ctx, SweepFilter{All: true}, false, time.Now())
	if n := count(t, d, `SELECT count(*) FROM messages`); n != 0 || len(got) != 2 {
		t.Errorf("all: left %d, folders %+v", n, got)
	}
	if !(SweepFilter{}).Empty() || (SweepFilter{Folder: "x"}).Empty() {
		t.Error("Empty() wrong")
	}
}

func TestCleanOrphansAndFTSRebuild(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	id := seed(t, d, "a", "INBOX", 1, 1, "")
	mustExec(t, d.SQL(), `INSERT INTO message_vectors(message_id, vector, model, dims) VALUES(?, x'00', 'm', 1)`, id)
	// Simulate orphans left by an older build with foreign keys off.
	mustExec(t, d.SQL(), `PRAGMA foreign_keys=OFF`)
	mustExec(t, d.SQL(), `INSERT INTO message_vectors(message_id, vector, model, dims) VALUES(999, x'00', 'm', 1)`)
	mustExec(t, d.SQL(), `INSERT INTO enrichment_queue(message_id) VALUES(998)`)
	mustExec(t, d.SQL(), `PRAGMA foreign_keys=ON`)
	// Corrupt the external-content FTS index with a row that has no message.
	mustExec(t, d.SQL(), `INSERT INTO messages_fts(rowid, subject, body_text, from_addr, from_name) VALUES(777, 'ghost', 'ghost', 'g', 'g')`)

	rep, err := d.Messages.CleanOrphans(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Vectors != 1 || rep.QueueEntries != 1 || !rep.FTSRebuilt {
		t.Fatalf("report = %+v", rep)
	}
	if n := count(t, d, `SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'ghost'`); n != 0 {
		t.Error("ghost FTS row survived rebuild")
	}
	if n := count(t, d, `SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'word'`); n != 1 {
		t.Error("real row lost in FTS rebuild")
	}
	rep, _ = d.Messages.CleanOrphans(ctx)
	if rep.Vectors != 0 || rep.QueueEntries != 0 || rep.FTSRebuilt {
		t.Errorf("second pass should be clean: %+v", rep)
	}
	if err := d.Messages.Compact(ctx, true); err != nil {
		t.Fatal(err)
	}
}

func TestPruneFoldersAndAccounts(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	seed(t, d, "a", "INBOX", 1, 1, "")
	seed(t, d, "a", "Old", 2, 1, "")
	seed(t, d, "gone", "INBOX", 3, 1, "")
	if err := d.Sync.Put(ctx, "a", "Empty", FolderSyncState{UIDValidity: 1}); err != nil {
		t.Fatal(err)
	}

	stale, err := d.Messages.PruneFolders(ctx, "a", []string{"INBOX"})
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 2 {
		t.Fatalf("stale = %+v (want Old and Empty)", stale)
	}
	if n := count(t, d, `SELECT count(*) FROM messages WHERE account='a'`); n != 1 {
		t.Errorf("account a has %d messages", n)
	}
	if n := count(t, d, `SELECT count(*) FROM messages WHERE account='gone'`); n != 1 {
		t.Error("PruneFolders must only touch its own account")
	}
	stale, _ = d.Messages.PruneAccounts(ctx, []string{"a"})
	if len(stale) != 1 || stale[0].Account != "gone" {
		t.Fatalf("accounts stale = %+v", stale)
	}
	if n := count(t, d, `SELECT count(*) FROM sync_state WHERE account='gone'`); n != 0 {
		t.Error("sync_state of removed account not pruned")
	}
}
