package intel

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// maxReplyGap bounds what counts as a reply time: a "reply" weeks later is a
// new conversation, not a slow answer, and would skew the average.
const maxReplyGap = 30 * 24 * time.Hour

// store writes the scan into imap.db.
type store struct{ db *sql.DB }

// scanState is one folder's progress.
type scanState struct {
	UIDValidity uint32
	LastUID     uint32
	Complete    bool
}

func (st *store) state(ctx context.Context, account, folder string) (scanState, error) {
	var s scanState
	var completed sql.NullInt64
	err := st.db.QueryRowContext(ctx, `SELECT uidvalidity, last_uid, completed_at FROM intel_scan WHERE account=? AND folder=?`,
		account, folder).Scan(&s.UIDValidity, &s.LastUID, &completed)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	s.Complete = completed.Valid
	return s, err
}

// applied reports what one batch changed.
type applied struct {
	New, Duplicate int
	Edges          int // messages whose graph edges were added
}

// apply records one batch and the folder's new progress in a single
// transaction, so a crash between batches never counts a message twice:
// either the batch and its progress are both stored, or neither is.
func (st *store) apply(ctx context.Context, account, folder string, validity uint32, b Batch, own map[string]bool, kg *kgTarget) (applied, error) {
	var res applied
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		return res, err
	}
	defer tx.Rollback() //nolint:errcheck

	for _, h := range b.Headers {
		if h.From.Addr == "" || h.Date.IsZero() {
			continue
		}
		hash := msgHash(h.MessageID)
		if hash == 0 {
			hash = keyHash(folder, h)
		}
		outgoing := own[h.From.Addr]
		var replyHash sql.NullInt64
		if outgoing {
			if rh := msgHash(h.InReplyTo); rh != 0 {
				replyHash = sql.NullInt64{Int64: rh, Valid: true}
			}
		}
		r, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO intel_messages(account, msg_hash, date, outgoing, reply_hash)
			VALUES(?,?,?,?,?)`, account, hash, h.Date.Unix(), boolInt(outgoing), replyHash)
		if err != nil {
			return res, err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			res.Duplicate++ // already seen in another folder or label (D28)
		} else {
			res.New++
			if err := st.profile(ctx, tx, account, hash, h, outgoing, own); err != nil {
				return res, err
			}
		}
		if kg != nil {
			// Claim the row (a write), so each message adds its edges once,
			// including on the one-time rescan after an upgrade.
			r, err := tx.ExecContext(ctx, `UPDATE intel_messages SET kg_done = 1 WHERE account = ? AND msg_hash = ? AND kg_done = 0`, account, hash)
			if err != nil {
				return res, err
			}
			if n, _ := r.RowsAffected(); n > 0 {
				if kg.w == nil || kg.w.tx != tx {
					kg.w = newKGWriter(ctx, tx)
				}
				if err := kg.w.addMessage(h, kg.owner, own); err != nil {
					return res, err
				}
				res.Edges++
			}
		}
	}
	return st.finish(ctx, tx, res, account, folder, validity, b)
}

// kgTarget carries the knowledge-graph state for a batch: the mailbox
// owner's address and a writer bound to the batch transaction.
type kgTarget struct {
	owner string
	w     *kgWriter
}

// profile updates sender profiles for one newly indexed message.
func (st *store) profile(ctx context.Context, tx *sql.Tx, account string, hash int64, h Header, outgoing bool, own map[string]bool) error {
	var senderID sql.NullInt64
	{
		when := h.Date.Unix()
		if outgoing {
			seen := map[string]bool{}
			for _, a := range append(append([]Address{}, h.To...), h.Cc...) {
				if a.Addr == "" || own[a.Addr] || seen[a.Addr] {
					return nil
				}
				seen[a.Addr] = true
				if _, err := tx.ExecContext(ctx, `INSERT INTO senders(address, name, domain, first_seen, last_seen, sent_count, dirty)
					VALUES(?,?,?,?,?,1,1)
					ON CONFLICT(address) DO UPDATE SET
						name = COALESCE(NULLIF(senders.name,''), excluded.name),
						first_seen = MIN(COALESCE(senders.first_seen, excluded.first_seen), excluded.first_seen),
						last_seen = MAX(COALESCE(senders.last_seen, excluded.last_seen), excluded.last_seen),
						sent_count = senders.sent_count + 1, dirty = 1, updated_at = unixepoch()`,
					a.Addr, a.Name, domainOf(a.Addr), when, when); err != nil {
					return err
				}
			}
			return nil
		}
		dkimP, dkimF := passFail(h.DKIM)
		dmarcP, dmarcF := passFail(h.DMARC)
		err := tx.QueryRowContext(ctx, `INSERT INTO senders(address, name, domain, first_seen, last_seen, message_count,
				list_count, bulk_count, auto_count, dkim_pass, dkim_fail, dmarc_pass, dmarc_fail, dirty)
			VALUES(?,?,?,?,?,1,?,?,?,?,?,?,?,1)
			ON CONFLICT(address) DO UPDATE SET
				name = CASE WHEN excluded.name <> '' THEN excluded.name ELSE senders.name END,
				first_seen = MIN(COALESCE(senders.first_seen, excluded.first_seen), excluded.first_seen),
				last_seen = MAX(COALESCE(senders.last_seen, excluded.last_seen), excluded.last_seen),
				message_count = senders.message_count + 1,
				list_count = senders.list_count + excluded.list_count,
				bulk_count = senders.bulk_count + excluded.bulk_count,
				auto_count = senders.auto_count + excluded.auto_count,
				dkim_pass = senders.dkim_pass + excluded.dkim_pass,
				dkim_fail = senders.dkim_fail + excluded.dkim_fail,
				dmarc_pass = senders.dmarc_pass + excluded.dmarc_pass,
				dmarc_fail = senders.dmarc_fail + excluded.dmarc_fail,
				dirty = 1, updated_at = unixepoch()
			RETURNING id`,
			h.From.Addr, h.From.Name, domainOf(h.From.Addr), when, when,
			boolInt(h.List), boolInt(h.Bulk), boolInt(h.Auto), dkimP, dkimF, dmarcP, dmarcF).Scan(&senderID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE intel_messages SET sender_id=? WHERE account=? AND msg_hash=?`,
			senderID, account, hash); err != nil {
			return err
		}
	}
	return nil
}

// finish records the folder's progress and commits the batch.
func (st *store) finish(ctx context.Context, tx *sql.Tx, res applied, account, folder string, validity uint32, b Batch) (applied, error) {
	var last uint32
	for _, h := range b.Headers {
		last = max(last, h.UID)
	}
	complete := !b.More
	if _, err := tx.ExecContext(ctx, `INSERT INTO intel_scan(account, folder, uidvalidity, last_uid, scanned, completed_at, updated_at)
		VALUES(?,?,?,?,?,CASE WHEN ? THEN unixepoch() END, unixepoch())
		ON CONFLICT(account, folder) DO UPDATE SET
			uidvalidity = excluded.uidvalidity,
			last_uid = MAX(intel_scan.last_uid, excluded.last_uid),
			scanned = intel_scan.scanned + excluded.scanned,
			completed_at = COALESCE(intel_scan.completed_at, excluded.completed_at),
			updated_at = unixepoch()`,
		account, folder, validity, last, len(b.Headers), complete); err != nil {
		return res, err
	}
	return res, tx.Commit()
}

// register records folders about to be scanned, so progress counts them
// before their first batch (backfill_complete must not be true while a
// folder has not started).
func (st *store) register(ctx context.Context, account string, folders []string) error {
	// Folders that left the scope (deleted, excluded) would otherwise stay
	// incomplete forever. Dropping their progress is safe: the D28 index keeps
	// a later rescan from counting anything twice.
	keep := map[string]bool{}
	for _, f := range folders {
		keep[f] = true
	}
	rows, err := st.db.QueryContext(ctx, `SELECT folder FROM intel_scan WHERE account=?`, account)
	if err != nil {
		return err
	}
	var gone []string
	for rows.Next() {
		var f string
		if rows.Scan(&f) == nil && !keep[f] {
			gone = append(gone, f)
		}
	}
	rows.Close()
	for _, f := range gone {
		if _, err := st.db.ExecContext(ctx, `DELETE FROM intel_scan WHERE account=? AND folder=?`, account, f); err != nil {
			return err
		}
	}
	for _, f := range folders {
		if _, err := st.db.ExecContext(ctx, `INSERT OR IGNORE INTO intel_scan(account, folder) VALUES(?,?)`, account, f); err != nil {
			return err
		}
	}
	return nil
}

// resetFolder restarts a folder's scan after a UIDVALIDITY change. The D28
// index keeps the rescan from counting any message twice.
func (st *store) resetFolder(ctx context.Context, account, folder string, validity uint32) error {
	_, err := st.db.ExecContext(ctx, `UPDATE intel_scan SET uidvalidity=?, last_uid=0, updated_at=unixepoch() WHERE account=? AND folder=?`,
		validity, account, folder)
	return err
}

// pairReplies matches our outgoing replies with the incoming message they
// answer (In-Reply-To hash) and adds the gap to that sender's reply stats.
// Each outgoing message is paired at most once. It returns how many were paired.
func (st *store) pairReplies(ctx context.Context, account string) (int, error) {
	// Read first, outside the write transaction: in WAL mode a transaction
	// that reads and then writes can fail with SQLITE_BUSY if another process
	// (run-rules) commits in between. The write transaction below starts with
	// a write and re-checks paired = 0, so a concurrent pass can't double-count.
	rows, err := st.db.QueryContext(ctx, `SELECT o.msg_hash, o.date - i.date, i.sender_id
		FROM intel_messages o JOIN intel_messages i ON i.account = o.account AND i.msg_hash = o.reply_hash
		WHERE o.account = ? AND o.outgoing = 1 AND o.paired = 0 AND i.outgoing = 0 AND i.sender_id IS NOT NULL`, account)
	if err != nil {
		return 0, err
	}
	type pair struct {
		hash, gap, sender int64
	}
	var pairs []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.hash, &p.gap, &p.sender); err != nil {
			rows.Close()
			return 0, err
		}
		pairs = append(pairs, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(pairs) == 0 {
		return 0, nil
	}
	tx, err := st.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	done := 0
	for _, p := range pairs {
		r, err := tx.ExecContext(ctx, `UPDATE intel_messages SET paired=1 WHERE account=? AND msg_hash=? AND paired=0`, account, p.hash)
		if err != nil {
			return 0, err
		}
		if n, _ := r.RowsAffected(); n == 0 {
			continue // paired meanwhile
		}
		done++
		if p.gap < 0 || time.Duration(p.gap)*time.Second > maxReplyGap {
			continue // clock skew or a much later follow-up: not a reply time
		}
		if _, err := tx.ExecContext(ctx, `UPDATE senders SET reply_count = reply_count + 1,
				reply_total_secs = reply_total_secs + ?,
				avg_reply_time = (reply_total_secs + ?) / (reply_count + 1), updated_at = unixepoch()
			WHERE id = ?`, p.gap, p.gap, p.sender); err != nil {
			return 0, err
		}
	}
	return done, tx.Commit()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func passFail(r string) (int, int) {
	switch r {
	case "pass":
		return 1, 0
	case "fail":
		return 0, 1
	}
	return 0, 0
}
