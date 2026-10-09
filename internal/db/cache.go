package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// FolderSyncState is the per-(account, folder) sync watermark.
type FolderSyncState struct {
	UIDValidity   uint32
	HighestModSeq uint64
	LastSynced    time.Time
}

// Get returns the stored state, or the zero value if the folder was never synced.
func (r *SyncRepo) Get(ctx context.Context, account, folder string) (FolderSyncState, error) {
	var st FolderSyncState
	var validity, modseq, last sql.NullInt64
	err := r.db.QueryRowContext(ctx,
		`SELECT uid_validity, highest_modseq, last_synced FROM sync_state WHERE account=? AND folder=?`,
		account, folder).Scan(&validity, &modseq, &last)
	if errors.Is(err, sql.ErrNoRows) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	st.UIDValidity = uint32(validity.Int64)
	st.HighestModSeq = uint64(modseq.Int64)
	if last.Valid {
		st.LastSynced = time.Unix(last.Int64, 0)
	}
	return st, nil
}

// Put records the state after a successful folder sync.
func (r *SyncRepo) Put(ctx context.Context, account, folder string, st FolderSyncState) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO sync_state(account, folder, uid_validity, highest_modseq, last_synced)
		VALUES(?, ?, ?, ?, unixepoch())
		ON CONFLICT(account, folder) DO UPDATE SET
			uid_validity=excluded.uid_validity,
			highest_modseq=excluded.highest_modseq,
			last_synced=excluded.last_synced`,
		account, folder, st.UIDValidity, int64(st.HighestModSeq))
	return err
}

// Attachment is metadata about one attachment (content is never cached).
type Attachment struct {
	Name string `json:"name"`
	MIME string `json:"mime"`
	Size int64  `json:"size"`
}

// CachedMessage is one message row in the cache.
type CachedMessage struct {
	Account      string
	Folder       string
	UID          uint32
	MessageID    string
	ThreadID     string
	Subject      string
	FromAddr     string
	FromName     string
	To           []string
	Cc           []string
	ReplyTo      string
	Date         time.Time
	InternalDate time.Time
	Flags        []string
	Size         int64
	BodyText     string
	BodyHTML     string
	BodySkipped  bool
	Attachments  []Attachment
}

// CachedUIDs returns the cached UIDs of a folder with their stored flags.
func (r *MessageRepo) CachedUIDs(ctx context.Context, account, folder string) (map[uint32][]string, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT uid, COALESCE(flags, '[]') FROM messages WHERE account=? AND folder=?`, account, folder)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uint32][]string{}
	for rows.Next() {
		var uid uint32
		var flags string
		if err := rows.Scan(&uid, &flags); err != nil {
			return nil, err
		}
		var f []string
		_ = json.Unmarshal([]byte(flags), &f)
		out[uid] = f
	}
	return out, rows.Err()
}

// InsertResult reports what Insert did.
type InsertResult struct {
	ID int64
	// Queued is false when another cached copy of the same Message-ID (e.g.
	// the same mail in INBOX and \All) is already queued for enrichment (D10).
	Queued bool
}

// Insert caches a message and queues it for enrichment unless a copy with the
// same Message-ID is already queued. An existing (account, folder, uid) row
// is replaced.
func (r *MessageRepo) Insert(ctx context.Context, m *CachedMessage) (InsertResult, error) {
	var res InsertResult
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback() //nolint:errcheck

	dup := false
	if m.MessageID != "" {
		var n int
		if err := tx.QueryRowContext(ctx, `
			SELECT count(*) FROM messages m JOIN enrichment_queue q ON q.message_id = m.id
			WHERE m.message_id = ? AND NOT (m.account = ? AND m.folder = ? AND m.uid = ?)`,
			m.MessageID, m.Account, m.Folder, m.UID).Scan(&n); err != nil {
			return res, err
		}
		dup = n > 0
	}
	status := "pending"
	if dup {
		status = "duplicate"
	}
	attach, _ := json.Marshal(m.Attachments)
	if m.Attachments == nil {
		attach = []byte("[]")
	}
	// Delete-then-insert keeps the FTS triggers and cascades consistent.
	if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE account=? AND folder=? AND uid=?`,
		m.Account, m.Folder, m.UID); err != nil {
		return res, err
	}
	out, err := tx.ExecContext(ctx, `
		INSERT INTO messages(account, folder, uid, message_id, thread_id, subject, from_addr, from_name,
			to_addrs, cc_addrs, reply_to, date, internal_date, flags, size, body_text, body_html,
			body_skipped, has_attachments, attachments, enrichment_status)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.Account, m.Folder, m.UID, nullStr(m.MessageID), nullStr(m.ThreadID), m.Subject, m.FromAddr, m.FromName,
		jsonList(m.To), jsonList(m.Cc), nullStr(m.ReplyTo), unixOr(m.Date, m.InternalDate), unixOr(m.InternalDate, m.Date),
		jsonList(m.Flags), m.Size, nullStr(m.BodyText), nullStr(m.BodyHTML),
		boolInt(m.BodySkipped), boolInt(len(m.Attachments) > 0), string(attach), status)
	if err != nil {
		return res, err
	}
	res.ID, _ = out.LastInsertId()
	if !dup {
		if _, err := tx.ExecContext(ctx, `INSERT INTO enrichment_queue(message_id) VALUES(?)`, res.ID); err != nil {
			return res, err
		}
		res.Queued = true
	}
	return res, tx.Commit()
}

// UpdateFlags stores new flags for a cached message. It reports whether a row
// was updated.
func (r *MessageRepo) UpdateFlags(ctx context.Context, account, folder string, uid uint32, flags []string) (bool, error) {
	out, err := r.db.ExecContext(ctx, `UPDATE messages SET flags=? WHERE account=? AND folder=? AND uid=?`,
		jsonList(flags), account, folder, uid)
	if err != nil {
		return false, err
	}
	n, _ := out.RowsAffected()
	return n > 0, nil
}

// DeleteUIDs removes cached messages (cache only — never the mailbox).
// Vectors and queue entries cascade.
func (r *MessageRepo) DeleteUIDs(ctx context.Context, account, folder string, uids []uint32) (int64, error) {
	var total int64
	for len(uids) > 0 {
		n := len(uids)
		if n > 500 {
			n = 500
		}
		batch := uids[:n]
		uids = uids[n:]
		args := []any{account, folder}
		for _, u := range batch {
			args = append(args, u)
		}
		out, err := r.db.ExecContext(ctx, fmt.Sprintf(
			`DELETE FROM messages WHERE account=? AND folder=? AND uid IN (%s)`,
			strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")), args...)
		if err != nil {
			return total, err
		}
		c, _ := out.RowsAffected()
		total += c
	}
	return total, nil
}

// DeleteFolder removes every cached message of a folder (UIDVALIDITY change).
func (r *MessageRepo) DeleteFolder(ctx context.Context, account, folder string) (int64, error) {
	out, err := r.db.ExecContext(ctx, `DELETE FROM messages WHERE account=? AND folder=?`, account, folder)
	if err != nil {
		return 0, err
	}
	return out.RowsAffected()
}

// FolderCount is the number of cached messages per (account, folder).
type FolderCount struct {
	Account string `json:"account"`
	Folder  string `json:"folder"`
	Count   int64  `json:"count"`
}

// Counts returns cached message counts per account and folder.
func (r *MessageRepo) Counts(ctx context.Context) ([]FolderCount, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT account, folder, count(*) FROM messages GROUP BY account, folder ORDER BY account, folder`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FolderCount
	for rows.Next() {
		var c FolderCount
		if err := rows.Scan(&c.Account, &c.Folder, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func jsonList(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func unixOr(t, fallback time.Time) int64 {
	if t.IsZero() {
		t = fallback
	}
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
