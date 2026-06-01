// Package sync handles IMAP synchronization: fetching new messages,
// updating flags, and maintaining sync state per account/folder.
package sync

import (
	"context"
	"log/slog"
	"time"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/imap"
)

// Syncer periodically fetches new messages from all accounts and folders.
type Syncer struct {
	cfg  *config.Config
	pool *imap.Pool
	db   *db.DB
	bus  *bus.Bus
	log  *slog.Logger
}

func New(cfg *config.Config, pool *imap.Pool, database *db.DB, b *bus.Bus, log *slog.Logger) *Syncer {
	return &Syncer{
		cfg:  cfg,
		pool: pool,
		db:   database,
		bus:  b,
		log:  log,
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
	return s.syncAccount(ctx, conn)
}

func (s *Syncer) syncAll(ctx context.Context) {
	for _, name := range s.pool.AccountNames() {
		if ctx.Err() != nil {
			return
		}
		conn, err := s.pool.Get(name)
		if err != nil {
			s.log.Error("get connection for sync", "account", name, "err", err)
			continue
		}
		if err := s.syncAccount(ctx, conn); err != nil {
			s.log.Error("sync account failed", "account", name, "err", err)
			s.bus.PublishAsync(bus.Event{
				Type:    bus.EventSyncError,
				Account: name,
				Payload: err.Error(),
			})
		}
	}
}

func (s *Syncer) syncAccount(ctx context.Context, conn *imap.Conn) error {
	folders := s.cfg.Sync.Folders
	if len(folders) == 0 {
		folders = []string{"INBOX"}
	}

	for _, folder := range folders {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.syncFolder(ctx, conn, folder); err != nil {
			s.log.Warn("sync folder failed", "account", conn.Account(), "folder", folder, "err", err)
		}
	}

	s.bus.PublishAsync(bus.Event{
		Type:    bus.EventSyncComplete,
		Account: conn.Account(),
	})
	return nil
}

func (s *Syncer) syncFolder(ctx context.Context, conn *imap.Conn, folder string) error {
	s.log.Debug("syncing folder", "account", conn.Account(), "folder", folder)

	conn.Lock()
	defer conn.Unlock()

	client := conn.Client()

	// Select the mailbox
	_, err := client.Select(folder, nil).Wait()
	if err != nil {
		return err
	}

	// Get last synced UID from state
	var lastUID uint32
	s.db.SQL().QueryRowContext(ctx, `
		SELECT COALESCE(last_uid, 0) FROM sync_state WHERE account=? AND folder=?
	`, conn.Account(), folder).Scan(&lastUID) //nolint:errcheck

	// Fetch messages newer than lastUID
	// Full implementation: use IMAP UID FETCH with UID range lastUID+1:*
	// This is the scaffold — the full fetch loop will be implemented in iteration 2
	s.log.Debug("sync state", "account", conn.Account(), "folder", folder, "last_uid", lastUID)

	// Update sync state
	s.db.SQL().ExecContext(ctx, `
		INSERT INTO sync_state(account, folder, last_synced)
		VALUES(?, ?, unixepoch())
		ON CONFLICT(account, folder) DO UPDATE SET last_synced=unixepoch()
	`, conn.Account(), folder) //nolint:errcheck

	s.bus.PublishAsync(bus.Event{
		Type:    bus.EventFolderSynced,
		Account: conn.Account(),
		Payload: folder,
	})
	return nil
}
