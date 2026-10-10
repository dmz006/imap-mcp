package intel

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strings"
	"time"
)

// Folder kinds for learning from moves (AGENT.md D34).
const (
	KindTrash   = "trash"
	KindJunk    = "junk"
	KindDiscard = "discard" // a configured rules.learn.discard_folders entry
)

// DiscardKind classifies a folder: Trash or Junk by SPECIAL-USE attribute,
// else by common name, or a configured discard folder; "" for every other
// folder.
func DiscardKind(name string, attrs []string, extra []string) string {
	if slices.Contains(attrs, `\Trash`) {
		return KindTrash
	}
	if slices.Contains(attrs, `\Junk`) {
		return KindJunk
	}
	base := name
	if i := strings.LastIndexAny(name, "/."); i >= 0 {
		base = name[i+1:]
	}
	switch strings.ToLower(base) {
	case "trash", "deleted items", "deleted messages":
		return KindTrash
	case "junk", "spam", "junk e-mail", "junk email", "bulk mail":
		return KindJunk
	}
	if slices.ContainsFunc(extra, func(x string) bool { return strings.EqualFold(strings.TrimSpace(x), name) }) {
		return KindDiscard
	}
	return ""
}

// folderKinds is one account's folder classification for a tick.
type folderKinds struct {
	kind map[string]string // folder → trash | junk | discard
	hold map[string]bool   // folders a new_sender rule holds mail in
}

func (k *folderKinds) discard(folder string) bool { return k != nil && k.kind[folder] != "" }

// rescueFrom reports whether a message leaving folder counts as a rescue:
// out of Junk or a hold folder (D34).
func (k *folderKinds) rescueFrom(folder string) bool {
	return k != nil && (k.kind[folder] == KindJunk || k.hold[folder])
}

// kinds classifies an account's folders and reads its hold folders.
func (sc *Scanner) kinds(ctx context.Context, account string, all []Folder) (*folderKinds, error) {
	k := &folderKinds{kind: map[string]string{}, hold: map[string]bool{}}
	for _, f := range all {
		if kind := DiscardKind(f.Name, f.Attrs, sc.discardFolders); kind != "" {
			k.kind[f.Name] = kind
		}
	}
	rows, err := sc.state.QueryContext(ctx, `SELECT DISTINCT folder FROM held_messages WHERE account = ? AND folder IS NOT NULL`, account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f string
		if err := rows.Scan(&f); err != nil {
			return nil, err
		}
		k.hold[f] = true
	}
	return k, rows.Err()
}

// locationScope is the discard folders the normal scan does not read (Junk,
// and on Gmail-style servers Trash and Spam, which are outside All Mail).
// They get a location-only pass. Folders in intelligence.exclude_folders are
// left alone.
func (sc *Scanner) locationScope(all []Folder, scanned []string, k *folderKinds) []string {
	var out []string
	for _, f := range all {
		if k.kind[f.Name] == "" || slices.Contains(scanned, f.Name) || slices.Contains(f.Attrs, `\Noselect`) {
			continue
		}
		if slices.ContainsFunc(sc.cfg.ExcludeFolders, func(x string) bool { return strings.EqualFold(x, f.Name) }) {
			continue
		}
		out = append(out, f.Name)
	}
	return out
}

// locate records that the scan saw an indexed message in folder and reports
// a rescue: the message was last seen in Junk or a hold folder and is now in
// a folder that is not a discard folder. A message the index does not know is
// ignored.
func (st *store) locate(ctx context.Context, tx *sql.Tx, account string, hash int64, folder string, k *folderKinds) error {
	var prev sql.NullString
	var sender sql.NullInt64
	err := tx.QueryRowContext(ctx, `SELECT folder, sender_id FROM intel_messages WHERE account = ? AND msg_hash = ?`, account, hash).Scan(&prev, &sender)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if prev.String == folder {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE intel_messages SET folder = ? WHERE account = ? AND msg_hash = ?`, folder, account, hash); err != nil {
		return err
	}
	if prev.Valid && k.rescueFrom(prev.String) && !k.discard(folder) && sender.Valid {
		return st.rescue(ctx, tx, account, sender.Int64)
	}
	return nil
}

// rescue trusts a sender the owner pulled mail back from Junk or a hold
// folder for, resolves their open anomalies and clears a dismissed
// suggestion (D34, D47).
func (st *store) rescue(ctx context.Context, tx *sql.Tx, account string, senderID int64) error {
	var addr string
	if err := tx.QueryRowContext(ctx, `SELECT address FROM senders WHERE id = ?`, senderID).Scan(&addr); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE senders SET trusted = 1, updated_at = unixepoch() WHERE address = ?`, addr); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE anomalies SET resolved = 1, resolved_at = COALESCE(resolved_at, unixepoch())
		WHERE sender = ? AND resolved = 0`, addr); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM learn_state WHERE account = ? AND target = ?`, account, addr)
	return err
}

// locateFolder runs the location-only pass over one Trash/Junk folder: it
// reads new messages' Message-IDs and records where they are. Nothing else
// is touched, so spam never reaches profiles, the graph or anomaly checks.
func (sc *Scanner) locateFolder(ctx context.Context, account, folder string, k *folderKinds) error {
	var validity, after uint32
	err := sc.state.QueryRowContext(ctx, `SELECT uidvalidity, last_uid FROM intel_locscan WHERE account = ? AND folder = ?`,
		account, folder).Scan(&validity, &after)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	batch := max(sc.cfg.BatchSize, 1)
	for ctx.Err() == nil {
		b, err := sc.src.Fetch(ctx, account, folder, after, batch)
		if err != nil {
			return err
		}
		if validity != 0 && b.UIDValidity != validity {
			validity, after = b.UIDValidity, 0
			continue
		}
		validity = b.UIDValidity
		tx, err := sc.state.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		for _, h := range b.Headers {
			if hash := msgHash(h.MessageID); hash != 0 {
				if err := sc.st.locate(ctx, tx, account, hash, folder, k); err != nil {
					tx.Rollback() //nolint:errcheck
					return err
				}
			}
			after = max(after, h.UID)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO intel_locscan(account, folder, uidvalidity, last_uid, updated_at)
			VALUES(?,?,?,?,unixepoch())
			ON CONFLICT(account, folder) DO UPDATE SET uidvalidity = excluded.uidvalidity, last_uid = excluded.last_uid, updated_at = unixepoch()`,
			account, folder, validity, after); err != nil {
			tx.Rollback() //nolint:errcheck
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if !b.More {
			return nil
		}
		sc.sleep(ctx, time.Duration(len(b.Headers))*time.Minute/time.Duration(max(sc.cfg.BackfillPerMinute, 1)))
	}
	return ctx.Err()
}
