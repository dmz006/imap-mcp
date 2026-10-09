// Package sync maintains the local mail cache (cache.db) from IMAP
// (AGENT.md D6, D8, D9, D10).
//
// Each cycle, per account and per configured folder:
//
//   - resolve the folder (SPECIAL-USE token such as \Sent, or a literal name)
//   - EXAMINE it (read-only); a changed UIDVALIDITY drops that folder's cache
//   - UID SEARCH SINCE <now - window_days> (INTERNALDATE) gives the server set
//   - new = server − cached: fetched newest-first in batches (envelope, flags,
//     size, INTERNALDATE, full body via BODY.PEEK[] under the size cap) and
//     queued for enrichment, de-duplicated by Message-ID
//   - gone = cached − server: expunged, moved or aged out of the window;
//     removed from the cache only, never from the mailbox
//   - flags: FETCH CHANGEDSINCE <modseq> where CONDSTORE is available, else a
//     FLAGS re-fetch of the window
//
// Every change is published on the bus (message.synced / message.updated /
// message.deleted, folder.synced, sync.complete).
package sync

import (
	"context"
	"log/slog"
	"slices"
	gosync "sync"
	"time"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/imap"
)

// fetchBatch is how many new messages are fetched per lock hold.
const fetchBatch = 25

// Syncer periodically refreshes the cache for all accounts and folders.
type Syncer struct {
	cfg  *config.Config
	pool *imap.Pool
	db   *db.DB
	bus  *bus.Bus
	log  *slog.Logger

	// newSource builds the mailbox view for a connection (overridable in tests).
	newSource func(*imap.Conn) Source
	// now is the clock (overridable in tests).
	now func() time.Time

	mu         gosync.Mutex
	stats      map[string]FolderStats // key: account + "\x00" + folder entry
	lastVacuum time.Time
	lastClean  CleanReport // accumulating during a cycle
	lastReport CleanReport // last completed pass
}

// CleanReport summarises one cleaning pass (D7): cache only, never the mailbox.
type CleanReport struct {
	StaleFolders []db.FolderCount `json:"stale_folders,omitempty"`
	Orphans      db.OrphanReport  `json:"orphans"`
	Vacuumed     bool             `json:"vacuumed"`
	At           time.Time        `json:"at"`
}

// FolderStats is the outcome of the last sync of one folder entry.
type FolderStats struct {
	Account    string    `json:"account"`
	Entry      string    `json:"entry"`  // config entry, e.g. \Sent
	Folder     string    `json:"folder"` // resolved mailbox name
	WindowDays int       `json:"window_days"`
	Cached     int       `json:"cached"`
	New        int       `json:"new"`
	Removed    int       `json:"removed"`
	FlagsDone  int       `json:"flags_updated"`
	CondStore  bool      `json:"condstore"`
	Rebuilt    bool      `json:"rebuilt,omitempty"`
	Error      string    `json:"error,omitempty"`
	At         time.Time `json:"at"`
}

func New(cfg *config.Config, pool *imap.Pool, database *db.DB, b *bus.Bus, log *slog.Logger) *Syncer {
	return &Syncer{
		cfg:        cfg,
		pool:       pool,
		db:         database,
		bus:        b,
		log:        log,
		newSource:  NewIMAPSource,
		now:        time.Now,
		stats:      map[string]FolderStats{},
		lastVacuum: time.Now(), // no VACUUM right at startup
	}
}

// Run starts the background sync loop. Triggers an initial full sync if configured,
// then repeats on the configured interval.
func (s *Syncer) Run(ctx context.Context) {
	if s.cfg.Sync.FullSyncOnStart {
		s.log.Info("starting full sync on all accounts")
		s.syncAll(ctx)
	}

	interval := time.Duration(s.cfg.Sync.IntervalMinutes) * time.Minute
	if interval <= 0 {
		interval = 15 * time.Minute
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.syncAll(ctx)
		}
	}
}

// SyncAccount triggers an immediate sync for a single account.
func (s *Syncer) SyncAccount(ctx context.Context, accountName string) error {
	conn, err := s.pool.Get(accountName)
	if err != nil {
		return err
	}
	return s.syncAccount(ctx, conn.Account(), s.newSource(conn))
}

// Stats returns the last sync outcome of every folder entry, sorted.
func (s *Syncer) Stats() []FolderStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]FolderStats, 0, len(s.stats))
	for _, st := range s.stats {
		out = append(out, st)
	}
	slices.SortFunc(out, func(a, b FolderStats) int {
		if a.Account != b.Account {
			return compare(a.Account, b.Account)
		}
		return compare(a.Entry, b.Entry)
	})
	return out
}

func compare(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (s *Syncer) record(st FolderStats) {
	s.mu.Lock()
	s.stats[st.Account+"\x00"+st.Entry] = st
	s.mu.Unlock()
}

func (s *Syncer) syncAll(ctx context.Context) {
	defer s.clean(ctx)
	for _, name := range s.pool.AccountNames() {
		if ctx.Err() != nil {
			return
		}
		conn, err := s.pool.Get(name)
		if err != nil {
			s.log.Error("get connection for sync", "account", name, "err", err)
			continue
		}
		if err := s.syncAccount(ctx, name, s.newSource(conn)); err != nil {
			s.log.Error("sync account failed", "account", name, "err", err)
			s.bus.PublishAsync(bus.Event{
				Type:    bus.EventSyncError,
				Account: name,
				Payload: err.Error(),
			})
		}
	}
}

func (s *Syncer) syncAccount(ctx context.Context, account string, src Source) error {
	acct, _ := s.cfg.Account(account)
	entries := s.cfg.SyncFolders(acct)

	folders, err := src.Folders(ctx)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	defer func() {
		// Folders dropped from config or gone from the server: drop their cache.
		keep := make([]string, 0, len(seen))
		for f := range seen {
			keep = append(keep, f)
		}
		if stale, err := s.db.Messages.PruneFolders(ctx, account, keep); err != nil {
			s.log.Warn("prune folders failed", "account", account, "err", err)
		} else {
			s.addStale(stale)
		}
	}()
	for _, entry := range entries {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		name, ok := resolveFolder(entry, folders)
		st := FolderStats{Account: account, Entry: entry, Folder: name, WindowDays: s.cfg.WindowDays(acct, entry), At: s.now()}
		if !ok {
			st.Error = "folder not found on server"
			s.log.Warn("sync folder not found", "account", account, "entry", entry)
			s.record(st)
			continue
		}
		if seen[name] {
			continue // two entries resolved to the same mailbox
		}
		seen[name] = true
		if entry == `\All` {
			s.log.Warn(`\All selected: every message is cached (copies in other folders are de-duplicated for enrichment)`, "account", account)
		}
		if err := s.syncFolder(ctx, src, &st); err != nil {
			st.Error = err.Error()
			s.log.Warn("sync folder failed", "account", account, "folder", name, "err", err)
		}
		s.record(st)
		s.bus.PublishAsync(bus.Event{Type: bus.EventFolderSynced, Account: account, Payload: st})
	}

	s.bus.PublishAsync(bus.Event{Type: bus.EventSyncComplete, Account: account})
	return nil
}

func (s *Syncer) addStale(stale []db.FolderCount) {
	if len(stale) == 0 {
		return
	}
	s.mu.Lock()
	s.lastClean.StaleFolders = append(s.lastClean.StaleFolders, stale...)
	s.mu.Unlock()
	for _, c := range stale {
		s.log.Info("dropped cache of folder no longer synced", "account", c.Account, "folder", c.Folder, "messages", c.Count)
	}
}

// clean runs after every sync cycle: drop accounts no longer configured,
// remove orphans, checkpoint the WAL, and VACUUM at most every
// vacuum_interval_hours. Cache only.
func (s *Syncer) clean(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	names := make([]string, 0, len(s.cfg.Accounts))
	for _, a := range s.cfg.Accounts {
		names = append(names, a.Name)
	}
	stale, err := s.db.Messages.PruneAccounts(ctx, names)
	if err != nil {
		s.log.Warn("prune accounts failed", "err", err)
	}
	s.addStale(stale)
	orphans, err := s.db.Messages.CleanOrphans(ctx)
	if err != nil {
		s.log.Warn("orphan cleanup failed", "err", err)
	}
	s.mu.Lock()
	interval := time.Duration(s.cfg.Sync.VacuumIntervalHours) * time.Hour
	vacuum := interval > 0 && s.now().Sub(s.lastVacuum) >= interval
	s.mu.Unlock()
	if err := s.db.Messages.Compact(ctx, vacuum); err != nil {
		s.log.Warn("cache compact failed", "vacuum", vacuum, "err", err)
		vacuum = false
	}
	s.mu.Lock()
	if vacuum {
		s.lastVacuum = s.now()
	}
	s.lastClean.Orphans, s.lastClean.Vacuumed, s.lastClean.At = orphans, vacuum, s.now()
	rep := s.lastClean
	s.lastClean = CleanReport{}
	s.mu.Unlock()
	s.publishClean(rep)
}

func (s *Syncer) publishClean(rep CleanReport) {
	s.mu.Lock()
	s.lastReport = rep
	s.mu.Unlock()
	s.bus.PublishAsync(bus.Event{Type: bus.EventCacheCleaned, Payload: rep})
}

// LastClean returns the most recent cleaning pass.
func (s *Syncer) LastClean() CleanReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastReport
}

// SweepResult is the outcome of an on-demand cache_sweep.
type SweepResult struct {
	DryRun  bool             `json:"dry_run"`
	Folders []db.FolderCount `json:"folders"`
	Total   int64            `json:"total"`
	Orphans *db.OrphanReport `json:"orphans,omitempty"`
	Note    string           `json:"note,omitempty"`
}

// Sweep is the on-demand cache_sweep (D7): with dryRun it only counts. A real
// sweep deletes the selected cached messages (never mailbox mail), resets
// their folders' sync state, cleans orphans and VACUUMs. Anything still inside
// the sync window comes back on the next cycle.
func (s *Syncer) Sweep(ctx context.Context, f db.SweepFilter, dryRun bool) (SweepResult, error) {
	res := SweepResult{DryRun: dryRun}
	counts, err := s.db.Messages.Sweep(ctx, f, dryRun, s.now())
	if err != nil {
		return res, err
	}
	res.Folders = counts
	for _, c := range counts {
		res.Total += c.Count
	}
	if dryRun {
		res.Note = "dry run: nothing deleted; repeat with dry_run=false to delete these cached copies (the mailbox is never touched)"
		return res, nil
	}
	orphans, err := s.db.Messages.CleanOrphans(ctx)
	if err != nil {
		return res, err
	}
	res.Orphans = &orphans
	if err := s.db.Messages.Compact(ctx, true); err != nil {
		return res, err
	}
	s.mu.Lock()
	s.lastVacuum = s.now()
	s.mu.Unlock()
	res.Note = "deleted from the cache only; messages still inside the sync window are re-fetched on the next cycle"
	s.bus.PublishAsync(bus.Event{Type: bus.EventCacheCleaned, Payload: res})
	return res, nil
}

// syncFolder brings one folder's cache in line with the server inside the
// window. st carries the folder identity in and the counters out.
func (s *Syncer) syncFolder(ctx context.Context, src Source, st *FolderStats) error {
	account, folder := st.Account, st.Folder

	status, err := src.Status(ctx, folder)
	if err != nil {
		return err
	}
	st.CondStore = status.CondStore
	prev, err := s.db.Sync.Get(ctx, account, folder)
	if err != nil {
		return err
	}
	if prev.UIDValidity != 0 && prev.UIDValidity != status.UIDValidity {
		n, err := s.db.Messages.DeleteFolder(ctx, account, folder)
		if err != nil {
			return err
		}
		s.log.Info("uidvalidity changed; folder cache rebuilt", "account", account, "folder", folder, "dropped", n)
		prev = db.FolderSyncState{}
		st.Rebuilt = true
	}

	y, m, d := s.now().AddDate(0, 0, -st.WindowDays).Date()
	since := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	server, err := src.SearchSince(ctx, folder, status.UIDValidity, since, s.cfg.Sync.KeepFlagged)
	if err != nil {
		return err
	}
	cached, err := s.db.Messages.CachedUIDs(ctx, account, folder)
	if err != nil {
		return err
	}

	onServer := make(map[uint32]bool, len(server))
	var fresh, existing []uint32
	for _, u := range server {
		onServer[u] = true
		if _, ok := cached[u]; ok {
			existing = append(existing, u)
		} else {
			fresh = append(fresh, u)
		}
	}
	var gone []uint32
	for u := range cached {
		if !onServer[u] {
			gone = append(gone, u)
		}
	}

	// Gone: expunged, moved, or aged out of the window. Cache only.
	if len(gone) > 0 {
		n, err := s.db.Messages.DeleteUIDs(ctx, account, folder, gone)
		if err != nil {
			return err
		}
		st.Removed = int(n)
		for _, u := range gone {
			s.bus.PublishAsync(bus.Event{Type: bus.EventMessageDeleted, Account: account, Payload: map[string]any{"folder": folder, "uid": u}})
		}
	}

	// Flags on messages we already have.
	changedSince := uint64(0)
	if status.CondStore && prev.HighestModSeq > 0 {
		changedSince = prev.HighestModSeq
	}
	updates, err := src.Flags(ctx, folder, status.UIDValidity, existing, changedSince)
	if err != nil {
		return err
	}
	for _, up := range updates {
		if sameFlags(cached[up.UID], up.Flags) {
			continue
		}
		ok, err := s.db.Messages.UpdateFlags(ctx, account, folder, up.UID, up.Flags)
		if err != nil {
			return err
		}
		if ok {
			st.FlagsDone++
			s.bus.PublishAsync(bus.Event{Type: bus.EventMessageUpdated, Account: account, Payload: map[string]any{"folder": folder, "uid": up.UID, "flags": up.Flags}})
		}
	}

	// New mail first: highest UIDs are the newest.
	slices.SortFunc(fresh, func(a, b uint32) int { return int(int64(b) - int64(a)) })
	maxBytes := int64(s.cfg.Sync.MaxMessageMB) << 20
	for len(fresh) > 0 {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n := min(fetchBatch, len(fresh))
		batch := fresh[:n]
		fresh = fresh[n:]
		msgs, err := src.Fetch(ctx, folder, status.UIDValidity, batch, maxBytes)
		if err != nil {
			return err
		}
		for i := range msgs {
			cm := toCached(account, folder, &msgs[i])
			res, err := s.db.Messages.Insert(ctx, cm)
			if err != nil {
				return err
			}
			st.New++
			s.bus.PublishAsync(bus.Event{Type: bus.EventMessageSynced, Account: account, Payload: map[string]any{
				"folder": folder, "uid": cm.UID, "id": res.ID, "queued": res.Queued,
			}})
		}
	}

	st.Cached = len(existing) + st.New
	return s.db.Sync.Put(ctx, account, folder, db.FolderSyncState{UIDValidity: status.UIDValidity, HighestModSeq: status.HighestModSeq})
}

func sameFlags(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := slices.Clone(a)
	y := slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

// toCached converts a fetched message into a cache row.
func toCached(account, folder string, f *Fetched) *db.CachedMessage {
	cm := &db.CachedMessage{
		Account:      account,
		Folder:       folder,
		UID:          f.UID,
		Flags:        f.Flags,
		Size:         f.Size,
		InternalDate: f.InternalDate,
		BodySkipped:  f.Raw == nil,
	}
	var refs []string
	if f.Raw != nil {
		body := parseBody(f.Raw)
		cm.BodyText, cm.BodyHTML, cm.Attachments, refs = body.Text, body.HTML, body.Attachments, body.References
	}
	if env := f.Envelope; env != nil {
		cm.MessageID = env.MessageID
		cm.Subject = decodeWord(env.Subject)
		cm.Date = env.Date
		if len(env.From) > 0 {
			cm.FromAddr = env.From[0].Addr()
			cm.FromName = decodeWord(env.From[0].Name)
		}
		for _, a := range env.To {
			cm.To = append(cm.To, a.Addr())
		}
		for _, a := range env.Cc {
			cm.Cc = append(cm.Cc, a.Addr())
		}
		if len(env.ReplyTo) > 0 {
			cm.ReplyTo = env.ReplyTo[0].Addr()
		}
		cm.ThreadID = threadID(refs, env.InReplyTo, env.MessageID)
	}
	return cm
}
