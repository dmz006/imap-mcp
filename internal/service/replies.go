package service

import (
	"context"
	"database/sql"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/dmz006/imap-mcp/internal/intel"
)

// Reply tracking (Q1; AGENT.md D32, D33). The header scan keeps the latest
// message of every person-to-person conversation in reply_threads, from the
// whole history. needs_reply lists conversations whose latest message is
// someone else's, addressed to the owner; awaiting_reply lists those where
// the owner wrote last. An item clears when the owner replies (an outgoing
// message in the index answers it), when its message carries \Answered, or
// when it is dismissed. Where a message is filed never clears it: rules file
// real conversations out of INBOX.

const (
	defaultReplyOlderDays  = 2
	defaultReplyWithinDays = 90
	maxReplyWithinDays     = 36500
	defaultReplyLimit      = 20
	maxReplyLimit          = 200
)

// ReplyParams selects reply items.
type ReplyParams struct {
	Account       string // "" = every account
	OlderThanDays int    // waiting at least this many days (0 = no minimum)
	WithinDays    int    // latest message at most this many days old (0 = 90)
	Limit         int    // 0 = 20
}

// ReplyItem is one conversation waiting on someone.
type ReplyItem struct {
	Account     string `json:"account"`
	ThreadID    string `json:"thread_id"` // for get_thread and dismiss_reply
	Counterpart string `json:"counterpart"`
	Name        string `json:"name,omitempty"`
	Subject     string `json:"subject,omitempty"`
	MessageRef  string `json:"message_ref,omitempty"` // Message-ID of the latest message
	Folder      string `json:"folder,omitempty"`
	UID         uint32 `json:"uid,omitempty"`
	LastDate    string `json:"last_date"` // RFC 3339 (UTC)
	DaysWaiting int    `json:"days_waiting"`

	threadHash, lastHash int64
}

// ReplyList is a needs_reply or awaiting_reply result.
type ReplyList struct {
	Count int         `json:"count"`
	Items []ReplyItem `json:"items"`
	// HistoryComplete is false while the header scan is still filling
	// reply_threads (first scan, or the one-time rescan after 0.16): the
	// list may be missing older conversations.
	HistoryComplete bool `json:"history_complete"`
}

// NeedsReply lists conversations whose latest message is someone else's,
// addressed to the owner, that the owner has not answered or dismissed.
func (s *Service) NeedsReply(ctx context.Context, p ReplyParams) (ReplyList, error) {
	return s.replyList(ctx, p, false, time.Now())
}

// AwaitingReply lists conversations where the owner wrote last and nobody
// has answered.
func (s *Service) AwaitingReply(ctx context.Context, p ReplyParams) (ReplyList, error) {
	return s.replyList(ctx, p, true, time.Now())
}

func (s *Service) replyList(ctx context.Context, p ReplyParams, outgoing bool, now time.Time) (ReplyList, error) {
	out := ReplyList{Items: []ReplyItem{}}
	if s.db == nil || s.db.StateSQL() == nil {
		return out, unavailable("the state database is not open in this mode")
	}
	if !s.cfg.Intel.On() {
		return out, unavailable("reply tracking needs intelligence (the header scan) enabled")
	}
	if p.OlderThanDays < 0 {
		return out, invalid("older_than_days must be 0 or more")
	}
	if p.WithinDays <= 0 {
		p.WithinDays = defaultReplyWithinDays
	}
	p.WithinDays = min(p.WithinDays, maxReplyWithinDays)
	if p.WithinDays < p.OlderThanDays {
		return out, invalid("within_days must be at least older_than_days")
	}
	if p.Limit <= 0 {
		p.Limit = defaultReplyLimit
	}
	p.Limit = min(p.Limit, maxReplyLimit)

	accounts := s.replyAccounts(p.Account)
	day := 24 * time.Hour
	olderThan := now.Add(-time.Duration(p.OlderThanDays) * day).Unix()
	within := now.Add(-time.Duration(p.WithinDays) * day).Unix()
	for _, account := range accounts {
		items, err := s.replyItems(ctx, account, outgoing, olderThan, within, p.Limit-len(out.Items), now)
		if err != nil {
			return out, err
		}
		out.Items = append(out.Items, items...)
		if len(out.Items) >= p.Limit {
			break
		}
	}
	slices.SortStableFunc(out.Items, func(a, b ReplyItem) int { return b.DaysWaiting - a.DaysWaiting })
	out.Count = len(out.Items)
	complete, err := s.replyHistoryComplete(ctx, accounts)
	out.HistoryComplete = complete
	return out, err
}

// replyAccounts resolves the account parameter ("" = every account).
func (s *Service) replyAccounts(account string) []string {
	if account != "" {
		return []string{s.accountName(account)}
	}
	var out []string
	for _, a := range s.cfg.Accounts {
		out = append(out, a.Name)
	}
	return out
}

// replyItems reads one account's open items, oldest first. Incoming items
// skip mail in Trash, Junk or a hold folder (D48), then keep known contacts
// and only clean first contacts, checked live along with \Answered.
func (s *Service) replyItems(ctx context.Context, account string, outgoing bool, olderThan, within int64, limit int, now time.Time) ([]ReplyItem, error) {
	if limit <= 0 {
		return nil, nil
	}
	args := []any{account}
	q := `SELECT t.thread_hash, COALESCE(t.thread_id,''), t.last_hash, COALESCE(t.counterpart,''), COALESCE(s.name,''), COALESCE(t.subject,''),
			COALESCE(t.message_ref,''), COALESCE(t.folder,''), COALESCE(t.uid,0), t.last_date,
			COALESCE(s.role,'unknown'), COALESCE(s.role_source,''), COALESCE(s.sent_count,0) + COALESCE(s.reply_count,0)
		FROM reply_threads t LEFT JOIN senders s ON s.address = t.counterpart
		LEFT JOIN intel_messages im ON im.account = t.account AND im.msg_hash = t.last_hash
		WHERE t.account = ? AND t.outgoing = ? AND t.last_date <= ? AND t.last_date >= ?
			AND COALESCE(t.dismissed_hash, 0) <> t.last_hash
			AND COALESCE(s.role,'unknown') NOT IN ('newsletter','bot')`
	out := 0
	if outgoing {
		out = 1
	}
	args = append(args, out, olderThan, within)
	if !outgoing {
		q += ` AND t.direct = 1 AND t.answered = 0
			AND NOT EXISTS (SELECT 1 FROM intel_messages o WHERE o.account = t.account AND o.outgoing = 1 AND o.reply_hash = t.last_hash)
			AND NOT EXISTS (SELECT 1 FROM held_messages h WHERE h.account = t.account AND h.msg_hash = t.last_hash AND h.released_at IS NULL)`
		gone, err := s.replyClearingFolders(ctx, account)
		if err != nil {
			return nil, err
		}
		if len(gone) > 0 {
			q += ` AND COALESCE(im.folder, t.folder, '') NOT IN (` + strings.TrimSuffix(strings.Repeat("?,", len(gone)), ",") + `)`
			for _, f := range gone {
				args = append(args, f)
			}
		}
	}
	q += ` ORDER BY t.last_date`
	rows, err := s.db.StateSQL().QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	// Read every row before any further query: the state database allows
	// one connection, so a query inside the loop would wait forever.
	type row struct {
		it        ReplyItem
		known     bool
		uid, last int64
	}
	var all []row
	for rows.Next() {
		var r row
		var written int64
		var role, roleSource string
		if err := rows.Scan(&r.it.threadHash, &r.it.ThreadID, &r.it.lastHash, &r.it.Counterpart, &r.it.Name, &r.it.Subject,
			&r.it.MessageRef, &r.it.Folder, &r.uid, &r.last, &role, &roleSource, &written); err != nil {
			rows.Close()
			return nil, err
		}
		r.known = outgoing || written > 0 || ((role == "personal" || role == "colleague") && strings.HasPrefix(roleSource, "signal:"))
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var items []ReplyItem
	firstContact := map[int64]bool{} // thread hash → needs the live header check
	for _, r := range all {
		it := r.it
		if !r.known {
			if !replyFirstContacts {
				continue
			}
			ok, err := s.cleanFirstContact(ctx, account, it.Counterpart, it.MessageRef)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			firstContact[it.threadHash] = true
		}
		it.Account, it.UID = account, uint32(r.uid)
		it.LastDate = time.Unix(r.last, 0).UTC().Format(time.RFC3339)
		it.DaysWaiting = int(now.Sub(time.Unix(r.last, 0)) / (24 * time.Hour))
		items = append(items, it)
		if len(items) >= limit {
			break
		}
	}
	if !outgoing {
		items = s.liveCheck(ctx, account, items, firstContact)
	}
	return items, nil
}

// replyFirstContacts lets first-time senders into needs_reply when they pass
// the D48 checks. Off until Q3's model second opinion can judge content
// (D49): header checks and the classify label alone let look-alike spam in.
var replyFirstContacts = false

// replyClearingFolders are the folders whose mail never needs a reply:
// Trash, Junk, configured discard folders and hold folders (D48).
func (s *Service) replyClearingFolders(ctx context.Context, account string) ([]string, error) {
	var out []string
	if s.pool != nil {
		kinds, err := s.discardFolders(account)
		if err != nil {
			slog.Debug("replies: list discard folders", "account", account, "err", err)
		}
		for f := range kinds {
			out = append(out, f)
		}
	}
	rows, err := s.db.StateSQL().QueryContext(ctx, `SELECT DISTINCT folder FROM held_messages WHERE account = ? AND folder IS NOT NULL`, account)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f string
		if rows.Scan(&f) == nil && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out, rows.Err()
}

// cleanFirstContact is the database half of the first-contact check (D48):
// the classify model called the message a conversation or personal mail,
// and the sender has no open anomaly other than new_sender (which every first
// contact has). The header half runs live in liveCheck.
func (s *Service) cleanFirstContact(ctx context.Context, account, sender, messageRef string) (bool, error) {
	cache := s.db.SQL()
	id := strings.Trim(messageRef, "<>")
	if cache == nil || id == "" {
		return false, nil
	}
	var hall sql.NullString
	if err := cache.QueryRowContext(ctx, `SELECT hall FROM messages WHERE account = ? AND message_id IN (?, ?) AND COALESCE(hall,'') <> '' LIMIT 1`,
		account, id, "<"+id+">").Scan(&hall); err != nil {
		return false, nil //nolint:nilerr // not cached or not classified: not a clean first contact
	}
	if h := strings.Trim(strings.ToLower(strings.TrimSpace(hall.String)), "<>"); h != "conversation" && h != "personal" {
		return false, nil
	}
	var open int
	err := s.db.StateSQL().QueryRowContext(ctx, `SELECT count(*) FROM anomalies WHERE sender = ? AND resolved = 0 AND anomaly_type <> 'new_sender'`,
		sender).Scan(&open)
	return open == 0, err
}

// liveCheck reads the listed messages on the server: it drops those the
// owner has since answered (\Answered, recorded so they are not checked
// again) and first contacts whose headers show any bulk or scam signal (the
// new-sender hold's checks, D48). If the server cannot be reached, answered
// checks are skipped and first contacts are dropped (they cannot be vetted).
func (s *Service) liveCheck(ctx context.Context, account string, items []ReplyItem, firstContact map[int64]bool) []ReplyItem {
	if len(items) == 0 {
		return items
	}
	var gate *newSenderGate
	if len(firstContact) > 0 {
		g, err := s.newSenderGate(account, 0)
		if err != nil {
			slog.Debug("replies: first-contact check unavailable", "account", account, "err", err)
		} else {
			gate = g
		}
	}
	key := func(folder string, uid uint32) string { return folder + "|" + strconv.FormatUint(uint64(uid), 10) }
	byFolder := map[string][]imaplib.UID{}
	for _, it := range items {
		if it.Folder != "" && it.UID > 0 {
			byFolder[it.Folder] = append(byFolder[it.Folder], imaplib.UID(it.UID))
		}
	}
	answered, clean := map[string]bool{}, map[string]bool{}
	if s.pool != nil {
		if c, err := s.pool.Resolve(account); err == nil {
			c.Lock()
			for folder, uids := range byFolder {
				if ctx.Err() != nil {
					break
				}
				if _, err := c.Client().Select(folder, &imaplib.SelectOptions{ReadOnly: true}).Wait(); err != nil {
					slog.Debug("replies: live check select", "account", account, "err", err)
					continue
				}
				opts := &imaplib.FetchOptions{UID: true, Flags: true}
				if gate != nil {
					opts.Envelope = true
					opts.BodySection = []*imaplib.FetchItemBodySection{{Specifier: imaplib.PartSpecifierHeader, HeaderFields: holdHeaderFields, Peek: true}}
				}
				msgs, err := c.Client().Fetch(imaplib.UIDSetNum(uids...), opts).Collect()
				if err != nil {
					slog.Debug("replies: live check fetch", "account", account, "err", err)
					continue
				}
				for _, m := range msgs {
					k := key(folder, uint32(m.UID))
					if slices.Contains(m.Flags, imaplib.FlagAnswered) {
						answered[k] = true
					}
					if gate != nil && m.Envelope != nil && len(m.Envelope.From) > 0 {
						real, score, _, err := gate.signals(m.Envelope, headerOf(m), bareAddr(m.Envelope.From[0].Addr()))
						if err == nil && (real || score == 0) {
							clean[k] = true
						}
					}
				}
			}
			c.Unlock()
		}
	}
	kept := items[:0]
	for _, it := range items {
		k := key(it.Folder, it.UID)
		if answered[k] {
			if _, err := s.db.StateSQL().ExecContext(ctx, `UPDATE reply_threads SET answered = 1 WHERE account = ? AND thread_hash = ? AND last_hash = ?`,
				account, it.threadHash, it.lastHash); err != nil {
				slog.Debug("replies: record answered", "account", account, "err", err)
			}
			continue
		}
		if firstContact[it.threadHash] && !clean[k] {
			continue
		}
		kept = append(kept, it)
	}
	return kept
}

// replyHistoryComplete reports whether every folder of the accounts has been
// scanned through, including the one-time rescan that fills reply_threads.
func (s *Service) replyHistoryComplete(ctx context.Context, accounts []string) (bool, error) {
	if len(accounts) == 0 {
		return false, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(accounts)), ",")
	args := make([]any, len(accounts))
	for i, a := range accounts {
		args[i] = a
	}
	var folders, done int
	err := s.db.StateSQL().QueryRowContext(ctx, `SELECT count(*),
			count(*) FILTER (WHERE completed_at IS NOT NULL AND last_uid >= COALESCE(rescan_until, 0))
		FROM intel_scan WHERE account IN (`+ph+`)`, args...).Scan(&folders, &done)
	if err != nil {
		return false, err
	}
	return folders > 0 && folders == done, nil
}

// DismissReply removes a conversation from the reply lists until a newer
// message arrives in it (D33).
func (s *Service) DismissReply(ctx context.Context, account, threadID string) (map[string]any, error) {
	if s.db == nil || s.db.StateSQL() == nil {
		return nil, unavailable("the state database is not open in this mode")
	}
	hash := intel.MsgHash(threadID)
	if hash == 0 {
		return nil, invalid("thread_id is required (from needs_reply or awaiting_reply)")
	}
	account = s.accountName(account)
	r, err := s.db.StateSQL().ExecContext(ctx, `UPDATE reply_threads SET dismissed_hash = last_hash, dismissed_at = unixepoch() WHERE account = ? AND thread_hash = ?`,
		account, hash)
	if err != nil {
		return nil, err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return nil, notFound("no conversation %s in account %s", threadID, account)
	}
	return map[string]any{"account": account, "thread_id": threadID, "dismissed": true}, nil
}
