package service

import (
	"context"
	"fmt"
	"mime"
	"slices"
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/dmz006/imap-mcp/internal/bus"
)

// heldRetention is how long held_messages rows are kept.
const heldRetention = 90 * 24 * time.Hour

// heldRow is one held message awaiting the digest.
type heldRow struct {
	sender, subject, reasons, folder string
	heldAt                           int64
}

// digestReplyLimit is how many waiting conversations the digest lists.
const digestReplyLimit = 25

// sendHoldDigests sends each account's daily digest (AGENT.md D31) when it
// is due: once a day, at the first full rule run at or after the configured
// hour, and only when there is something to report — mail held since the
// last digest, or conversations waiting on the owner (Q1). The digest is a
// summary message APPENDed to the account's INBOX (no mail is sent) and a
// hold.digest event with the account and counts only.
func (s *Service) sendHoldDigests(now time.Time) error {
	if !s.cfg.Rules.HoldDigestOn() || now.Hour() < s.cfg.Rules.HoldDigestHour {
		return nil
	}
	st := s.db.StateSQL()
	if _, err := st.Exec(`DELETE FROM held_messages WHERE held_at < ?`, now.Add(-heldRetention).Unix()); err != nil {
		return err
	}
	rows, err := st.Query(`SELECT DISTINCT account FROM held_messages WHERE digested_at IS NULL AND released_at IS NULL`)
	if err != nil {
		return err
	}
	var accounts []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			rows.Close()
			return err
		}
		accounts = append(accounts, a)
	}
	rows.Close()
	if s.cfg.Intel.On() { // any account may have conversations waiting
		for _, a := range s.cfg.Accounts {
			if !slices.Contains(accounts, a.Name) {
				accounts = append(accounts, a.Name)
			}
		}
	}
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	var errs []string
	// New setup findings (D52), read once when the first digest is due.
	// Global ones go into the first digest sent.
	var fresh []Finding
	freshRead, globalShown := false, false
	for _, account := range accounts {
		var last int64
		if err := st.QueryRow(`SELECT max(COALESCE((SELECT max(digested_at) FROM held_messages WHERE account = ?), 0),
				COALESCE((SELECT sent_at FROM digest_log WHERE account = ?), 0))`, account, account).Scan(&last); err != nil {
			return err
		}
		if last >= dayStart {
			continue // already sent today
		}
		if !freshRead {
			freshRead = true
			if f, err := s.newFindings(context.Background()); err != nil {
				errs = append(errs, "setup check: "+err.Error())
			} else {
				fresh = f
			}
		}
		var setup []Finding
		for _, f := range fresh {
			if f.Account == account || (f.Account == "" && !globalShown) {
				setup = append(setup, f)
			}
		}
		sent, err := s.sendHoldDigest(account, now, setup)
		if err != nil {
			errs = append(errs, account+": "+err.Error())
			continue
		}
		if sent {
			for _, f := range setup {
				if f.Account == "" {
					globalShown = true
				}
			}
			if err := s.markFindings(context.Background(), setup, now); err != nil {
				errs = append(errs, account+": "+err.Error())
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("digest: %s", strings.Join(errs, "; "))
	}
	return nil
}

// sendHoldDigest sends one account's digest; sent is false when there was
// nothing to report.
func (s *Service) sendHoldDigest(account string, now time.Time, setup []Finding) (sent bool, err error) {
	st := s.db.StateSQL()
	rows, err := st.Query(`SELECT COALESCE(sender,''), COALESCE(subject,''), COALESCE(reasons,''), COALESCE(folder,''), held_at
		FROM held_messages WHERE account = ? AND digested_at IS NULL AND released_at IS NULL ORDER BY held_at`, account)
	if err != nil {
		return false, err
	}
	var held []heldRow
	for rows.Next() {
		var h heldRow
		if err := rows.Scan(&h.sender, &h.subject, &h.reasons, &h.folder, &h.heldAt); err != nil {
			rows.Close()
			return false, err
		}
		held = append(held, h)
	}
	rows.Close()
	var waiting ReplyList
	if s.cfg.Intel.On() {
		if waiting, err = s.replyList(context.Background(), ReplyParams{Account: account,
			OlderThanDays: defaultReplyOlderDays, WithinDays: defaultReplyWithinDays, Limit: digestReplyLimit}, false, now); err != nil {
			return false, err
		}
	}
	learned, err := s.digestLearned(account)
	if err != nil {
		return false, err
	}
	ids, err := s.SuggestIdentities(context.Background())
	if err != nil {
		return false, err
	}
	learned.identities = ids.Candidates
	learned.setup = setup
	if len(held) == 0 && waiting.Count == 0 && len(learned.suggested) == 0 && len(learned.created) == 0 && len(learned.identities) == 0 && len(setup) == 0 {
		return false, nil
	}

	conn, err := s.pool.Resolve(account)
	if err != nil {
		return false, err
	}
	owner := ""
	for _, a := range s.cfg.Accounts {
		if a.Name == conn.Account() {
			owner = bareAddr(a.Auth.Username)
		}
	}
	raw := digestMessage(owner, account, held, waiting, learned, now)
	conn.Lock()
	cmd := conn.Client().Append("INBOX", int64(len(raw)), &imaplib.AppendOptions{Time: now})
	_, werr := cmd.Write(raw)
	cerr := cmd.Close()
	_, aerr := cmd.Wait()
	conn.Unlock()
	for _, e := range []error{werr, cerr, aerr} {
		if e != nil {
			return false, fmt.Errorf("append digest: %w", e)
		}
	}
	if _, err := st.Exec(`UPDATE held_messages SET digested_at = ? WHERE account = ? AND digested_at IS NULL AND released_at IS NULL`,
		now.Unix(), account); err != nil {
		return false, err
	}
	if _, err := st.Exec(`INSERT INTO digest_log(account, sent_at) VALUES(?,?)
		ON CONFLICT(account) DO UPDATE SET sent_at = excluded.sent_at`, account, now.Unix()); err != nil {
		return false, err
	}
	if b := s.pool.Bus(); b != nil {
		b.Publish(bus.Event{Type: bus.EventHoldDigest, Account: account,
			Payload: map[string]any{"account": account, "held": len(held), "waiting": waiting.Count, "suggested": len(learned.suggested)}})
	}
	return true, nil
}

// digestMessage renders the digest as a plain-text message from and to the
// account owner: mail held from first-time senders, then conversations
// waiting on the owner.
func digestMessage(owner, account string, held []heldRow, waiting ReplyList, learned digestLearning, now time.Time) []byte {
	oneLine := strings.NewReplacer("\r", " ", "\n", " ")
	var body strings.Builder
	if len(held) > 0 {
		fmt.Fprintf(&body, "imap-mcp held %d message(s) from first-time senders that looked like bulk mail or scams.\r\n", len(held))
		body.WriteString("Nothing was deleted. Move a message back to your inbox and that sender is never held again.\r\n\r\n")
		for _, h := range held {
			fmt.Fprintf(&body, "- %s  %s\r\n  \"%s\"\r\n  why: %s -> %s\r\n\r\n",
				time.Unix(h.heldAt, 0).In(now.Location()).Format("Jan 2 15:04"), h.sender,
				oneLine.Replace(h.subject), h.reasons, h.folder)
		}
	}
	if waiting.Count > 0 {
		fmt.Fprintf(&body, "Waiting on you: %d conversation(s) where someone wrote to you %d or more days ago and you have not replied.\r\n",
			waiting.Count, defaultReplyOlderDays)
		body.WriteString("Reply, or dismiss one with dismiss_reply, and it leaves this list.\r\n")
		if !waiting.HistoryComplete {
			body.WriteString("(Your mail history is still being scanned; this list may be incomplete.)\r\n")
		}
		body.WriteString("\r\n")
		for _, w := range waiting.Items {
			who := w.Counterpart
			if w.Name != "" {
				who = w.Name + " <" + w.Counterpart + ">"
			}
			fmt.Fprintf(&body, "- %d day(s)  %s\r\n  \"%s\"\r\n\r\n", w.DaysWaiting, oneLine.Replace(who), oneLine.Replace(w.Subject))
		}
	}
	if len(learned.suggested) > 0 {
		fmt.Fprintf(&body, "Suggested rules: %d sender(s) whose mail you mostly move to Trash or Junk.\r\n", len(learned.suggested))
		body.WriteString("Create one with create_rule (it starts inactive), or say no with dismiss_suggestion.\r\n\r\n")
		for i, sg := range learned.suggested {
			if i == digestSuggestLimit {
				fmt.Fprintf(&body, "...and %d more (suggest_rules lists them all).\r\n\r\n", len(learned.suggested)-i)
				break
			}
			what := "trash"
			if sg.Action == "move" {
				what = "move to " + sg.Dest
			}
			fmt.Fprintf(&body, "- %s: you discarded %d of %d -> %s\r\n\r\n", sg.Target, sg.Discarded, sg.Received, what)
		}
	}
	if len(learned.created) > 0 {
		fmt.Fprintf(&body, "Rules created from your moves since the last digest: %d.\r\n", len(learned.created))
		body.WriteString("Delete one with delete_rule and it is never created again.\r\n\r\n")
		for _, c := range learned.created {
			fmt.Fprintf(&body, "- rule %d: %s\r\n", c.ruleID, c.target)
		}
		body.WriteString("\r\n")
	}
	if len(learned.identities) > 0 {
		fmt.Fprintf(&body, "Is this you? %d address(es) look like your own.\r\n", len(learned.identities))
		body.WriteString("Confirm with confirm_identity (it then counts as you everywhere) or reject with reject_identity.\r\n\r\n")
		for _, id := range learned.identities {
			fmt.Fprintf(&body, "- %s: %s\r\n", id.Address, id.Evidence)
		}
		body.WriteString("\r\n")
	}
	if len(learned.setup) > 0 {
		fmt.Fprintf(&body, "Setup: %d new finding(s) from the setup check.\r\n\r\n", len(learned.setup))
		for _, f := range learned.setup {
			fmt.Fprintf(&body, "- %s\r\n  Fix: %s\r\n\r\n", f.What, f.Fix)
		}
	}
	body.WriteString("-- \r\nimap-mcp daily digest\r\n")

	var parts []string
	if len(held) > 0 {
		parts = append(parts, fmt.Sprintf("Held for review: %d new-sender message(s)", len(held)))
	}
	if waiting.Count > 0 {
		parts = append(parts, fmt.Sprintf("Waiting on you: %d", waiting.Count))
	}
	if n := len(learned.suggested); n > 0 {
		parts = append(parts, fmt.Sprintf("Suggested rules: %d", n))
	}
	if n := len(learned.created); n > 0 {
		parts = append(parts, fmt.Sprintf("Rules created: %d", n))
	}
	if n := len(learned.identities); n > 0 {
		parts = append(parts, fmt.Sprintf("Is this you: %d", n))
	}
	subject := strings.Join(parts, "; ")
	var msg strings.Builder
	if owner != "" {
		fmt.Fprintf(&msg, "From: imap-mcp <%s>\r\nTo: <%s>\r\n", owner, owner)
	} else {
		msg.WriteString("From: imap-mcp\r\n")
	}
	fmt.Fprintf(&msg, "Subject: %s\r\nDate: %s\r\nMessage-ID: <hold-digest-%s-%d@imap-mcp.invalid>\r\n",
		mime.QEncoding.Encode("utf-8", subject), now.Format(time.RFC1123Z), account, now.Unix())
	msg.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
	msg.WriteString(body.String())
	return []byte(msg.String())
}

// digestLearning is the digest's learning-from-moves part (Q2).
type digestLearning struct {
	suggested []Suggestion // open suggestions (suggest mode)
	created   []struct {
		target string
		ruleID int64
	} // learned rules created since the last digest
	identities []Identity // addresses that may be the owner's (D50)
	setup      []Finding  // new setup-check findings (D52)
}

func (s *Service) digestLearned(account string) (digestLearning, error) {
	var out digestLearning
	if !s.cfg.Intel.On() {
		return out, nil
	}
	ctx := context.Background()
	list, err := s.suggestions(ctx, account, false)
	if err != nil {
		return out, err
	}
	for _, sg := range list {
		if sg.Status != learnStatusCreated {
			out.suggested = append(out.suggested, sg)
		}
	}
	rows, err := s.db.StateSQL().QueryContext(ctx, `SELECT target, COALESCE(rule_id,0) FROM learn_state
		WHERE account = ? AND status = ? AND updated_at > COALESCE((SELECT sent_at FROM digest_log WHERE account = ?), 0)
		ORDER BY target`, account, learnStatusCreated, account)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var c struct {
			target string
			ruleID int64
		}
		if err := rows.Scan(&c.target, &c.ruleID); err != nil {
			return out, err
		}
		out.created = append(out.created, c)
	}
	return out, rows.Err()
}

// HoldStatus is the new-sender hold's state for /api/health: settings and
// counts only, never senders, subjects or account names.
type HoldStatus struct {
	HoldDigest     bool   `json:"hold_digest"`
	HoldDigestHour int    `json:"hold_digest_hour"`
	Held           int    `json:"held"`                  // held, not released, in the last 90 days
	AwaitingDigest int    `json:"awaiting_digest"`       // held since the last digest
	LastDigest     string `json:"last_digest,omitempty"` // RFC 3339 (UTC)
}

// HoldStatus reports the digest settings and held-mail counts.
func (s *Service) HoldStatus() (HoldStatus, error) {
	st := HoldStatus{HoldDigest: s.cfg.Rules.HoldDigestOn(), HoldDigestHour: s.cfg.Rules.HoldDigestHour}
	if s.db == nil || s.db.StateSQL() == nil {
		return st, unavailable("the state database is not open in this mode")
	}
	var last int64
	err := s.db.StateSQL().QueryRow(`SELECT
			count(*) FILTER (WHERE released_at IS NULL),
			count(*) FILTER (WHERE released_at IS NULL AND digested_at IS NULL),
			COALESCE(max(digested_at), 0)
		FROM held_messages`).Scan(&st.Held, &st.AwaitingDigest, &last)
	if err != nil {
		return st, err
	}
	if last > 0 {
		st.LastDigest = time.Unix(last, 0).UTC().Format(time.RFC3339)
	}
	return st, nil
}
