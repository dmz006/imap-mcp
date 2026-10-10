package service

import (
	"fmt"
	"mime"
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

// sendHoldDigests sends each account's held-mail digest (AGENT.md D31) when
// it is due: once a day, at the first full rule run at or after the
// configured hour, and only when something was held since the last one. The
// digest is a summary message APPENDed to the account's INBOX (no mail is
// sent) and a hold.digest event with the account and count only.
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
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Unix()
	var errs []string
	for _, account := range accounts {
		var last int64
		if err := st.QueryRow(`SELECT COALESCE(max(digested_at), 0) FROM held_messages WHERE account = ?`, account).Scan(&last); err != nil {
			return err
		}
		if last >= dayStart {
			continue // already sent today
		}
		if err := s.sendHoldDigest(account, now); err != nil {
			errs = append(errs, account+": "+err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("hold digest: %s", strings.Join(errs, "; "))
	}
	return nil
}

func (s *Service) sendHoldDigest(account string, now time.Time) error {
	st := s.db.StateSQL()
	rows, err := st.Query(`SELECT COALESCE(sender,''), COALESCE(subject,''), COALESCE(reasons,''), COALESCE(folder,''), held_at
		FROM held_messages WHERE account = ? AND digested_at IS NULL AND released_at IS NULL ORDER BY held_at`, account)
	if err != nil {
		return err
	}
	var held []heldRow
	for rows.Next() {
		var h heldRow
		if err := rows.Scan(&h.sender, &h.subject, &h.reasons, &h.folder, &h.heldAt); err != nil {
			rows.Close()
			return err
		}
		held = append(held, h)
	}
	rows.Close()
	if len(held) == 0 {
		return nil
	}

	conn, err := s.pool.Resolve(account)
	if err != nil {
		return err
	}
	owner := ""
	for _, a := range s.cfg.Accounts {
		if a.Name == conn.Account() {
			owner = bareAddr(a.Auth.Username)
		}
	}
	raw := holdDigestMessage(owner, account, held, now)
	conn.Lock()
	cmd := conn.Client().Append("INBOX", int64(len(raw)), &imaplib.AppendOptions{Time: now})
	_, werr := cmd.Write(raw)
	cerr := cmd.Close()
	_, aerr := cmd.Wait()
	conn.Unlock()
	for _, e := range []error{werr, cerr, aerr} {
		if e != nil {
			return fmt.Errorf("append digest: %w", e)
		}
	}
	if _, err := st.Exec(`UPDATE held_messages SET digested_at = ? WHERE account = ? AND digested_at IS NULL AND released_at IS NULL`,
		now.Unix(), account); err != nil {
		return err
	}
	if b := s.pool.Bus(); b != nil {
		b.Publish(bus.Event{Type: bus.EventHoldDigest, Account: account,
			Payload: map[string]any{"account": account, "held": len(held)}})
	}
	return nil
}

// holdDigestMessage renders the digest as a plain-text message from and to
// the account owner.
func holdDigestMessage(owner, account string, held []heldRow, now time.Time) []byte {
	var body strings.Builder
	fmt.Fprintf(&body, "imap-mcp held %d message(s) from first-time senders that looked like bulk mail or scams.\r\n", len(held))
	body.WriteString("Nothing was deleted. Move a message back to your inbox and that sender is never held again.\r\n\r\n")
	for _, h := range held {
		fmt.Fprintf(&body, "- %s  %s\r\n  \"%s\"\r\n  why: %s -> %s\r\n\r\n",
			time.Unix(h.heldAt, 0).In(now.Location()).Format("Jan 2 15:04"), h.sender,
			strings.NewReplacer("\r", " ", "\n", " ").Replace(h.subject), h.reasons, h.folder)
	}
	body.WriteString("-- \r\nimap-mcp new-sender hold (new_sender rule)\r\n")

	subject := fmt.Sprintf("Held for review: %d new-sender message(s)", len(held))
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
