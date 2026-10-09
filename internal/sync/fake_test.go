package sync

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	gosync "sync"
	"testing"
	"time"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
)

// fakeMsg is one message in a fakeSource folder.
type fakeMsg struct {
	uid      uint32
	internal time.Time
	flags    []string
	modseq   uint64
	msgID    string
	size     int64
}

type fakeFolder struct {
	attrs    []string
	validity uint32
	modseq   uint64
	msgs     []*fakeMsg
}

// fakeSource is an in-memory Source with CONDSTORE semantics.
type fakeSource struct {
	mu        gosync.Mutex
	condstore bool
	folders   map[string]*fakeFolder
	// observations
	lastSince        time.Time
	lastChangedSince map[string]uint64
	fetchOrder       []uint32
	bodiesFetched    int
}

func (f *fakeSource) Folders(context.Context) ([]Folder, error) {
	var out []Folder
	for n, fo := range f.folders {
		out = append(out, Folder{Name: n, Attrs: fo.attrs})
	}
	return out, nil
}

func (f *fakeSource) folder(name string, validity uint32) (*fakeFolder, error) {
	fo := f.folders[name]
	if fo == nil {
		return nil, fmt.Errorf("no folder %s", name)
	}
	if validity != 0 && fo.validity != validity {
		return nil, ErrUIDValidityChanged
	}
	return fo, nil
}

func (f *fakeSource) Status(_ context.Context, name string) (FolderStatus, error) {
	fo, err := f.folder(name, 0)
	if err != nil {
		return FolderStatus{}, err
	}
	st := FolderStatus{UIDValidity: fo.validity}
	if f.condstore {
		st.HighestModSeq, st.CondStore = fo.modseq, true
	}
	return st, nil
}

func (f *fakeSource) SearchSince(_ context.Context, name string, v uint32, since time.Time, orFlagged bool) ([]uint32, error) {
	fo, err := f.folder(name, v)
	if err != nil {
		return nil, err
	}
	f.lastSince = since
	var out []uint32
	for _, m := range fo.msgs {
		if !m.internal.Before(since) || (orFlagged && slices.Contains(m.flags, `\Flagged`)) {
			out = append(out, m.uid)
		}
	}
	return out, nil
}

func (f *fakeSource) Fetch(_ context.Context, name string, v uint32, uids []uint32, maxBytes int64) ([]Fetched, error) {
	fo, err := f.folder(name, v)
	if err != nil {
		return nil, err
	}
	var out []Fetched
	for _, u := range uids {
		f.fetchOrder = append(f.fetchOrder, u)
		for _, m := range fo.msgs {
			if m.uid != u {
				continue
			}
			fe := Fetched{UID: u, Flags: m.flags, InternalDate: m.internal, Size: m.size,
				Envelope: &imaplib.Envelope{MessageID: m.msgID, Subject: fmt.Sprintf("subject %d", u),
					From: []imaplib.Address{{Name: "Sender", Mailbox: "s", Host: "example.com"}}}}
			if maxBytes <= 0 || m.size <= maxBytes {
				fe.Raw = []byte(fmt.Sprintf("Message-ID: <%s>\r\nContent-Type: text/plain\r\n\r\nbody of %d\r\n", m.msgID, u))
				f.bodiesFetched++
			}
			out = append(out, fe)
		}
	}
	return out, nil
}

func (f *fakeSource) Flags(_ context.Context, name string, v uint32, uids []uint32, changedSince uint64) ([]FlagUpdate, error) {
	fo, err := f.folder(name, v)
	if err != nil {
		return nil, err
	}
	if f.lastChangedSince == nil {
		f.lastChangedSince = map[string]uint64{}
	}
	f.lastChangedSince[name] = changedSince
	var out []FlagUpdate
	for _, u := range uids {
		for _, m := range fo.msgs {
			if m.uid == u && (changedSince == 0 || m.modseq > changedSince) {
				out = append(out, FlagUpdate{UID: u, Flags: m.flags})
			}
		}
	}
	return out, nil
}

type harness struct {
	s      *Syncer
	src    *fakeSource
	db     *db.DB
	events chan bus.Event
	now    time.Time
}

func newHarness(t *testing.T, condstore bool, cfgMut func(*config.Config)) *harness {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(db.Options{Path: filepath.Join(dir, "imap.db")}, db.Options{Path: filepath.Join(dir, "cache.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	cfg := &config.Config{
		Accounts: []config.AccountConfig{{Name: "personal"}},
		Sync:     config.SyncConfig{WindowDays: 30, MaxMessageMB: 1, Folders: []string{"INBOX", `\Sent`}},
	}
	if cfgMut != nil {
		cfgMut(cfg)
	}
	b := bus.New()
	h := &harness{db: d, events: make(chan bus.Event, 10000), now: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)}
	b.SubscribeAll(func(e bus.Event) { h.events <- e })
	h.src = &fakeSource{condstore: condstore, folders: map[string]*fakeFolder{
		"INBOX":     {validity: 7, modseq: 10},
		"Sent Mail": {attrs: []string{`\Sent`}, validity: 9, modseq: 5},
		"Archive":   {attrs: []string{`\Archive`}, validity: 3},
	}}
	h.s = New(cfg, nil, d, b, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.s.now = func() time.Time { return h.now }
	return h
}

func (h *harness) add(folder string, uid uint32, daysAgo int, msgID string, flags ...string) *fakeMsg {
	fo := h.src.folders[folder]
	fo.modseq++
	m := &fakeMsg{uid: uid, internal: h.now.AddDate(0, 0, -daysAgo), flags: flags, msgID: msgID, modseq: fo.modseq, size: 1000}
	fo.msgs = append(fo.msgs, m)
	return m
}

func (h *harness) sync(t *testing.T) {
	t.Helper()
	if err := h.s.syncAccount(context.Background(), "personal", h.src); err != nil {
		t.Fatal(err)
	}
	for _, st := range h.s.Stats() {
		if st.Error != "" {
			t.Fatalf("folder %s: %s", st.Entry, st.Error)
		}
	}
}

func (h *harness) cached(t *testing.T, folder string) []uint32 {
	t.Helper()
	m, err := h.db.Messages.CachedUIDs(context.Background(), "personal", folder)
	if err != nil {
		t.Fatal(err)
	}
	var out []uint32
	for u := range m {
		out = append(out, u)
	}
	slices.Sort(out)
	return out
}

func (h *harness) drain() map[bus.EventType]int {
	time.Sleep(20 * time.Millisecond) // PublishAsync
	out := map[bus.EventType]int{}
	for {
		select {
		case e := <-h.events:
			out[e.Type]++
		default:
			return out
		}
	}
}

func TestSyncWindowNewGoneAndOrder(t *testing.T) {
	h := newHarness(t, false, nil)
	h.add("INBOX", 1, 40, "old@x")  // outside window
	h.add("INBOX", 2, 10, "a@x")    //
	h.add("INBOX", 3, 1, "b@x")     //
	h.add("Sent Mail", 5, 2, "s@x") // resolved via \Sent
	h.add("Archive", 9, 1, "arc@x") // not configured
	h.sync(t)

	if got := h.cached(t, "INBOX"); !slices.Equal(got, []uint32{2, 3}) {
		t.Fatalf("INBOX cached = %v, want [2 3]", got)
	}
	if got := h.cached(t, "Sent Mail"); !slices.Equal(got, []uint32{5}) {
		t.Fatalf("Sent cached = %v", got)
	}
	if got := h.cached(t, "Archive"); len(got) != 0 {
		t.Fatalf("Archive must not be synced: %v", got)
	}
	if want := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC); !h.src.lastSince.Equal(want) {
		t.Errorf("SINCE = %v, want %v", h.src.lastSince, want)
	}
	// Newest first within a folder.
	if h.src.fetchOrder[0] != 3 || h.src.fetchOrder[1] != 2 {
		t.Errorf("fetch order = %v, want newest first", h.src.fetchOrder)
	}
	ev := h.drain()
	if ev[bus.EventMessageSynced] != 3 || ev[bus.EventFolderSynced] != 2 || ev[bus.EventSyncComplete] != 1 {
		t.Errorf("events = %v", ev)
	}

	// Expunge 2 on the server, age nothing; add 4. Second cycle: +4, -2.
	fo := h.src.folders["INBOX"]
	fo.msgs = slices.DeleteFunc(fo.msgs, func(m *fakeMsg) bool { return m.uid == 2 })
	h.add("INBOX", 4, 0, "c@x")
	h.sync(t)
	if got := h.cached(t, "INBOX"); !slices.Equal(got, []uint32{3, 4}) {
		t.Fatalf("INBOX after cycle 2 = %v, want [3 4]", got)
	}
	ev = h.drain()
	if ev[bus.EventMessageDeleted] != 1 || ev[bus.EventMessageSynced] != 1 {
		t.Errorf("cycle 2 events = %v", ev)
	}

	// Shrink the window to 0 days: everything older than today ages out (cache only).
	h.s.cfg.Sync.WindowDays = 0
	h.s.cfg.Sync.FolderWindowDays = map[string]int{"INBOX": 1}
	h.sync(t)
	if got := h.cached(t, "INBOX"); !slices.Equal(got, []uint32{3, 4}) {
		t.Fatalf("1-day window keeps uid 3 (1 day old) and 4: got %v", got)
	}
	h.s.cfg.Sync.FolderWindowDays = map[string]int{"INBOX": 0}
	h.s.cfg.Sync.WindowDays = 0 // falls back to default 30 → grow again
	h.sync(t)
	if got := h.cached(t, "INBOX"); !slices.Equal(got, []uint32{3, 4}) {
		t.Fatalf("got %v", got)
	}
	if len(h.src.folders["INBOX"].msgs) != 3 {
		t.Fatal("sync must never modify the mailbox")
	}
}

func TestSyncFlagsWithoutCondstore(t *testing.T) {
	h := newHarness(t, false, nil)
	m := h.add("INBOX", 1, 1, "a@x")
	h.sync(t)
	m.flags = []string{`\Seen`}
	h.sync(t)
	if h.src.lastChangedSince["INBOX"] != 0 {
		t.Errorf("without CONDSTORE changedSince must be 0, got %d", h.src.lastChangedSince["INBOX"])
	}
	assertFlags(t, h, "INBOX", 1, `["\\Seen"]`)
}

func TestSyncFlagsWithCondstore(t *testing.T) {
	h := newHarness(t, true, nil)
	m := h.add("INBOX", 1, 1, "a@x")
	h.add("INBOX", 2, 1, "b@x")
	h.sync(t)
	base := h.src.folders["INBOX"].modseq
	h.src.folders["INBOX"].modseq++
	m.flags, m.modseq = []string{`\Flagged`}, h.src.folders["INBOX"].modseq
	h.sync(t)
	if h.src.lastChangedSince["INBOX"] != base {
		t.Errorf("changedSince = %d, want stored modseq %d", h.src.lastChangedSince["INBOX"], base)
	}
	assertFlags(t, h, "INBOX", 1, `["\\Flagged"]`)
	ev := h.drain()
	if ev[bus.EventMessageUpdated] != 1 {
		t.Errorf("updated events = %d, want 1 (only the changed message)", ev[bus.EventMessageUpdated])
	}
}

func assertFlags(t *testing.T, h *harness, folder string, uid uint32, want string) {
	t.Helper()
	var got string
	h.db.SQL().QueryRow(`SELECT flags FROM messages WHERE folder=? AND uid=?`, folder, uid).Scan(&got)
	if got != want {
		t.Errorf("flags = %s, want %s", got, want)
	}
}

func TestSyncUIDValidityRebuild(t *testing.T) {
	h := newHarness(t, true, nil)
	h.add("INBOX", 1, 1, "a@x")
	h.add("INBOX", 2, 1, "b@x")
	h.sync(t)
	fo := h.src.folders["INBOX"]
	fo.validity = 99
	fo.msgs = nil
	h.add("INBOX", 1, 1, "z@x") // UID 1 reused for a different message
	h.sync(t)
	if got := h.cached(t, "INBOX"); !slices.Equal(got, []uint32{1}) {
		t.Fatalf("after rebuild = %v", got)
	}
	var mid string
	h.db.SQL().QueryRow(`SELECT message_id FROM messages WHERE folder='INBOX' AND uid=1`).Scan(&mid)
	if mid != "z@x" {
		t.Errorf("uid 1 is %q, want the new message", mid)
	}
	for _, st := range h.s.Stats() {
		if st.Entry == "INBOX" && !st.Rebuilt {
			t.Error("stats should report rebuild")
		}
	}
}

func TestSyncDedupAndBodyCap(t *testing.T) {
	h := newHarness(t, false, func(c *config.Config) { c.Sync.Folders = []string{"INBOX", `\Archive`} })
	h.add("INBOX", 1, 1, "same@x")
	h.add("Archive", 1, 1, "same@x")
	big := h.add("INBOX", 2, 1, "big@x")
	big.size = 5 << 20 // over the 1 MB cap
	h.sync(t)

	var queued, dups int
	h.db.SQL().QueryRow(`SELECT count(*) FROM enrichment_queue`).Scan(&queued)
	h.db.SQL().QueryRow(`SELECT count(*) FROM messages WHERE enrichment_status='duplicate'`).Scan(&dups)
	if queued != 2 || dups != 1 {
		t.Errorf("queued=%d duplicates=%d, want 2 and 1", queued, dups)
	}
	var skipped int
	var body *string
	h.db.SQL().QueryRow(`SELECT body_skipped, body_text FROM messages WHERE uid=2`).Scan(&skipped, &body)
	if skipped != 1 || body != nil {
		t.Errorf("oversize message: skipped=%d body=%v", skipped, body)
	}
	var text string
	h.db.SQL().QueryRow(`SELECT body_text FROM messages WHERE folder='INBOX' AND uid=1`).Scan(&text)
	if text != "body of 1\r\n" {
		t.Errorf("body_text = %q", text)
	}
	// FTS sees cached bodies.
	var hits int
	h.db.SQL().QueryRow(`SELECT count(*) FROM messages_fts WHERE messages_fts MATCH 'body'`).Scan(&hits)
	if hits != 2 {
		t.Errorf("fts hits = %d, want 2", hits)
	}
}

func TestSyncMissingFolderAndAccountOverride(t *testing.T) {
	h := newHarness(t, false, func(c *config.Config) {
		c.Accounts[0].Sync = &config.AccountSyncConfig{Folders: []string{"INBOX", "Nope"}, WindowDays: 5}
	})
	h.add("INBOX", 1, 3, "a@x")
	h.add("INBOX", 2, 7, "b@x")
	h.add("Sent Mail", 3, 1, "s@x")
	if err := h.s.syncAccount(context.Background(), "personal", h.src); err != nil {
		t.Fatal(err)
	}
	if got := h.cached(t, "INBOX"); !slices.Equal(got, []uint32{1}) {
		t.Errorf("account window 5 days: got %v", got)
	}
	if got := h.cached(t, "Sent Mail"); len(got) != 0 {
		t.Errorf("account folder list replaces global; Sent must not sync: %v", got)
	}
	var nope FolderStats
	for _, st := range h.s.Stats() {
		if st.Entry == "Nope" {
			nope = st
		}
	}
	if nope.Error == "" {
		t.Error("missing folder should be reported in stats")
	}
}

func TestSyncUIDValidityChangeMidCycleAborts(t *testing.T) {
	h := newHarness(t, false, nil)
	h.add("INBOX", 1, 1, "a@x")
	src := &flipSource{fakeSource: h.src}
	if err := h.s.syncAccount(context.Background(), "personal", src); err != nil {
		t.Fatal(err)
	}
	var inbox FolderStats
	for _, st := range h.s.Stats() {
		if st.Entry == "INBOX" {
			inbox = st
		}
	}
	if inbox.Error != ErrUIDValidityChanged.Error() {
		t.Errorf("error = %q", inbox.Error)
	}
	st, _ := h.db.Sync.Get(context.Background(), "personal", "INBOX")
	if st.UIDValidity != 0 {
		t.Error("aborted cycle must not record sync state")
	}
}

// flipSource changes UIDVALIDITY right after Status, as a server would after
// a folder is deleted and recreated mid-cycle.
type flipSource struct{ *fakeSource }

func (f *flipSource) Status(ctx context.Context, name string) (FolderStatus, error) {
	st, err := f.fakeSource.Status(ctx, name)
	f.folders[name].validity++
	return st, err
}

func TestResolveFolder(t *testing.T) {
	folders := []Folder{{Name: "INBOX"}, {Name: "[Gmail]/Sent Mail", Attrs: []string{`\HasNoChildren`, `\Sent`}}, {Name: "Archive"}}
	cases := map[string]string{"INBOX": "INBOX", "inbox": "INBOX", `\Sent`: "[Gmail]/Sent Mail", `\sent`: "[Gmail]/Sent Mail", "Archive": "Archive"}
	for entry, want := range cases {
		if got, ok := resolveFolder(entry, folders); !ok || got != want {
			t.Errorf("%q → %q,%v want %q", entry, got, ok, want)
		}
	}
	for _, miss := range []string{`\Archive`, "archive", "Nope"} {
		if _, ok := resolveFolder(miss, folders); ok {
			t.Errorf("%q should not resolve", miss)
		}
	}
}

func TestKeepFlaggedOutsideWindow(t *testing.T) {
	h := newHarness(t, false, func(c *config.Config) { c.Sync.KeepFlagged = true })
	h.add("INBOX", 1, 90, "old-flagged@x", `\Flagged`)
	h.add("INBOX", 2, 90, "old@x")
	h.add("INBOX", 3, 1, "new@x")
	h.sync(t)
	if got := h.cached(t, "INBOX"); !slices.Equal(got, []uint32{1, 3}) {
		t.Fatalf("keep_flagged: cached %v, want [1 3]", got)
	}
	// Unflag on the server: it ages out of the cache on the next cycle.
	h.src.folders["INBOX"].msgs[0].flags = nil
	h.sync(t)
	if got := h.cached(t, "INBOX"); !slices.Equal(got, []uint32{3}) {
		t.Fatalf("after unflag: %v", got)
	}
}

func TestFolderDroppedFromConfigIsPruned(t *testing.T) {
	h := newHarness(t, false, nil)
	h.add("INBOX", 1, 1, "a@x")
	h.add("Sent Mail", 2, 1, "s@x")
	h.sync(t)
	h.s.cfg.Sync.Folders = []string{"INBOX"}
	h.sync(t)
	if got := h.cached(t, "Sent Mail"); len(got) != 0 {
		t.Fatalf("Sent still cached after removal from config: %v", got)
	}
	if got := h.cached(t, "INBOX"); len(got) != 1 {
		t.Fatalf("INBOX lost: %v", got)
	}
}

func TestCleanAndVacuumInterval(t *testing.T) {
	h := newHarness(t, false, func(c *config.Config) { c.Sync.VacuumIntervalHours = 24 })
	h.add("INBOX", 1, 1, "a@x")
	h.sync(t)
	h.s.lastVacuum = h.now
	h.s.clean(context.Background())
	if h.s.LastClean().Vacuumed {
		t.Error("vacuumed before the interval elapsed")
	}
	h.now = h.now.Add(25 * time.Hour)
	h.s.clean(context.Background())
	if !h.s.LastClean().Vacuumed {
		t.Error("did not vacuum after the interval")
	}
	// Accounts removed from config are pruned.
	h.s.cfg.Accounts = nil
	h.s.clean(context.Background())
	if got := h.cached(t, "INBOX"); len(got) != 0 {
		t.Errorf("removed account still cached: %v", got)
	}
	ev := h.drain()
	if ev[bus.EventCacheCleaned] < 3 {
		t.Errorf("cache.cleaned events = %d", ev[bus.EventCacheCleaned])
	}
}

func TestSweepThroughSyncer(t *testing.T) {
	h := newHarness(t, false, nil)
	h.add("INBOX", 1, 1, "a@x")
	h.add("INBOX", 2, 2, "b@x")
	h.sync(t)
	res, err := h.s.Sweep(context.Background(), db.SweepFilter{Account: "personal"}, true)
	if err != nil || !res.DryRun || res.Total != 2 {
		t.Fatalf("dry run = %+v, %v", res, err)
	}
	res, err = h.s.Sweep(context.Background(), db.SweepFilter{Account: "personal"}, false)
	if err != nil || res.Total != 2 || res.Orphans == nil {
		t.Fatalf("sweep = %+v, %v", res, err)
	}
	if got := h.cached(t, "INBOX"); len(got) != 0 {
		t.Fatal("sweep left rows")
	}
	h.sync(t) // still inside the window → re-fetched
	if got := h.cached(t, "INBOX"); len(got) != 2 {
		t.Fatalf("re-fetch after sweep = %v", got)
	}
	if len(h.src.folders["INBOX"].msgs) != 2 {
		t.Fatal("sweep touched the mailbox")
	}
}

func TestEnrichmentLaneAssignment(t *testing.T) {
	h := newHarness(t, false, func(c *config.Config) { c.Sync.WindowDays = 10 })
	h.add("INBOX", 5, 1, "first@x")
	h.sync(t) // first sync of the folder → backfill
	h.add("INBOX", 6, 0, "arrived@x")
	h.add("INBOX", 2, 20, "older@x") // outside the 10-day window for now
	h.sync(t)                        // uid 6 > previous max 5 → new mail
	h.s.cfg.Sync.WindowDays = 30
	h.sync(t) // window grew: uid 2 ≤ max → backfill

	lanes := map[string]int{}
	rows, err := h.db.SQL().Query(`SELECT m.message_id, q.lane FROM enrichment_queue q JOIN messages m ON m.id = q.message_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var lane int
		rows.Scan(&id, &lane)
		lanes[id] = lane
	}
	want := map[string]int{"first@x": 1, "arrived@x": 0, "older@x": 1}
	for id, l := range want {
		if lanes[id] != l {
			t.Errorf("%s lane = %d, want %d (all: %v)", id, lanes[id], l, lanes)
		}
	}
}
