package service

import (
	"context"
	"database/sql"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/dmz006/imap-mcp/internal/intel"
	imapsync "github.com/dmz006/imap-mcp/internal/sync"
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
			AND COALESCE(s.role,'unknown') NOT IN ('newsletter','bot','self')`
	out := 0
	if outgoing {
		out = 1
	}
	args = append(args, out, olderThan, within)
	if !outgoing {
		// Automated senders never need a reply, even ones the owner once wrote
		// to (D51): vendor role, or mostly list/bulk/auto-submitted mail.
		q += ` AND COALESCE(s.role,'unknown') <> 'vendor'
			AND NOT (COALESCE(s.message_count,0) > 0
				AND (COALESCE(s.list_count,0) + COALESCE(s.bulk_count,0) + COALESCE(s.auto_count,0)) * 2 > COALESCE(s.message_count,0))
			AND t.direct = 1 AND t.answered = 0
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
	me := s.me(ctx)
	var items []ReplyItem
	firstContact := map[int64]bool{} // thread hash → needs the live header check
	for _, r := range all {
		it := r.it
		if me.has(it.Counterpart) { // the owner's other address (D50)
			continue
		}
		if !outgoing && (intel.IsNoReply(it.Counterpart) || isInvite(it.Subject, it.MessageRef) || s.automatedHall(ctx, account, it.MessageRef)) {
			continue // D51
		}
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

// isInvite reports a calendar invitation or update by its subject or its
// Message-ID (D51); liveCheck also looks for a text/calendar part.
func isInvite(subject, messageRef string) bool {
	s := strings.ToLower(strings.TrimSpace(subject))
	for _, p := range []string{"invitation:", "updated invitation", "invitation from google calendar", "accepted:", "declined:",
		"tentatively accepted:", "canceled event", "cancelled event", "event canceled", "new event:"} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return strings.HasPrefix(strings.ToLower(messageRef), "calendar-")
}

// automatedHall reports a message the classify model labelled transactional,
// notification or alert (D51). Messages outside the cache are not labelled.
func (s *Service) automatedHall(ctx context.Context, account, messageRef string) bool {
	cache := s.db.SQL()
	id := strings.Trim(messageRef, "<>")
	if cache == nil || id == "" {
		return false
	}
	var hall sql.NullString
	if err := cache.QueryRowContext(ctx, `SELECT hall FROM messages WHERE account = ? AND message_id IN (?, ?) AND COALESCE(hall,'') <> '' LIMIT 1`,
		account, id, "<"+id+">").Scan(&hall); err != nil {
		return false
	}
	switch strings.Trim(strings.ToLower(strings.TrimSpace(hall.String)), "<>") {
	case "transactional", "notification", "alert":
		return true
	}
	return false
}

// cachedText returns a cached message's text body, if the cache has it.
func (s *Service) cachedText(ctx context.Context, account, messageRef string) (string, bool) {
	cache := s.db.SQL()
	id := strings.Trim(messageRef, "<>")
	if cache == nil || id == "" {
		return "", false
	}
	var text, html sql.NullString
	if err := cache.QueryRowContext(ctx, `SELECT body_text, body_html FROM messages WHERE account = ? AND message_id IN (?, ?)
		AND COALESCE(body_skipped,0) = 0 LIMIT 1`, account, id, "<"+id+">").Scan(&text, &html); err != nil {
		return "", false
	}
	if text.String != "" {
		return text.String, true
	}
	if html.String != "" {
		return stripTags(html.String), true
	}
	return "", false
}

var tagRe = regexp.MustCompile(`(?s)<(script|style)[^>]*>.*?</(script|style)>|<br\s*/?>|</(p|div|tr|li)>|<[^>]+>`)

// stripTags turns HTML into rough text: block ends become new lines.
func stripTags(html string) string {
	return html2text.Replace(tagRe.ReplaceAllStringFunc(html, func(t string) string {
		if strings.HasPrefix(t, "<br") || strings.HasPrefix(t, "</") {
			return "\n"
		}
		return ""
	}))
}

var html2text = strings.NewReplacer("&nbsp;", " ", "&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'")

// hasCalendarPart reports a text/calendar or application/ics part.
func hasCalendarPart(bs imaplib.BodyStructure) bool {
	found := false
	bs.Walk(func(path []int, part imaplib.BodyStructure) bool {
		mt := strings.ToLower(part.MediaType())
		if mt == "text/calendar" || mt == "application/ics" {
			found = true
		}
		return !found
	})
	return found
}

// isForward reports a subject that forwards a message.
func isForward(subject string) bool {
	s := strings.ToLower(strings.TrimSpace(subject))
	return strings.HasPrefix(s, "fwd:") || strings.HasPrefix(s, "fw:") || strings.HasPrefix(s, "fwd :")
}

// forwardMarkers start the forwarded part of a message body.
var forwardMarkers = []string{
	"---------- forwarded message", "begin forwarded message", "-----original message-----",
	"________________________________", "-------- original message", "-------- forwarded message",
}

// hasOwnNote reports whether a forward carries text of the sender's own
// above the forwarded message (D51). Signatures and "Sent from my ..." lines
// do not count.
func hasOwnNote(text string) bool {
	lower := strings.ToLower(text)
	cut := len(lower)
	for _, m := range forwardMarkers {
		if i := strings.Index(lower, m); i >= 0 && i < cut {
			cut = i
		}
	}
	for _, line := range strings.Split(text[:cut], "\n") {
		l := strings.TrimSpace(line)
		ll := strings.ToLower(l)
		switch {
		case l == "", l == "--", strings.HasPrefix(ll, "sent from my"), strings.HasPrefix(ll, "get outlook for"),
			strings.HasPrefix(l, ">"), strings.HasPrefix(ll, "from:"), strings.HasPrefix(ll, "on ") && strings.HasSuffix(ll, "wrote:"):
			continue
		}
		return true
	}
	return false
}

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
	// Forwards need their text (D51): from the cache when it has the body,
	// else read live below.
	noted := map[int64]bool{}       // thread hash → forward with a note of its own
	liveFwd := map[string][]int64{} // folder|uid → forwards read live
	needBody := map[string][]imaplib.UID{}
	for _, it := range items {
		if it.Folder != "" && it.UID > 0 {
			byFolder[it.Folder] = append(byFolder[it.Folder], imaplib.UID(it.UID))
		}
		if isForward(it.Subject) {
			if text, ok := s.cachedText(ctx, account, it.MessageRef); ok {
				noted[it.threadHash] = hasOwnNote(text)
			} else if it.Folder != "" && it.UID > 0 {
				needBody[it.Folder] = append(needBody[it.Folder], imaplib.UID(it.UID))
				k := key(it.Folder, it.UID)
				liveFwd[k] = append(liveFwd[k], it.threadHash)
			}
		}
	}
	answered, clean, invite := map[string]bool{}, map[string]bool{}, map[string]bool{}
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
				opts := &imaplib.FetchOptions{UID: true, Flags: true, BodyStructure: &imaplib.FetchItemBodyStructure{}}
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
					if m.BodyStructure != nil && hasCalendarPart(m.BodyStructure) {
						invite[k] = true
					}
					if gate != nil && m.Envelope != nil && len(m.Envelope.From) > 0 {
						real, score, _, err := gate.signals(m.Envelope, headerOf(m), bareAddr(m.Envelope.From[0].Addr()))
						if err == nil && (real || score == 0) {
							clean[k] = true
						}
					}
				}
				if uids := needBody[folder]; len(uids) > 0 {
					bodies, err := c.Client().Fetch(imaplib.UIDSetNum(uids...), &imaplib.FetchOptions{UID: true,
						BodySection: []*imaplib.FetchItemBodySection{{Peek: true}}}).Collect()
					if err != nil {
						slog.Debug("replies: forward body fetch", "account", account, "err", err)
					}
					for _, m := range bodies {
						for _, bs := range m.BodySection {
							text, html := imapsync.PlainText(bs.Bytes)
							if text == "" {
								text = stripTags(html)
							}
							for _, th := range liveFwd[key(folder, uint32(m.UID))] {
								noted[th] = hasOwnNote(text)
							}
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
		if invite[k] {
			continue // a calendar invitation (D51)
		}
		if note, checked := noted[it.threadHash]; isForward(it.Subject) && checked && !note {
			continue // a bare forward (D51)
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
