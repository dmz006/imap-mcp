package intel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dmz006/imap-mcp/internal/bus"
)

// Anomaly types (AGENT.md D22).
const (
	AnomNewSender       = "new_sender"
	AnomAuthFailure     = "auth_failure"
	AnomLookalikeDomain = "lookalike_domain"
	AnomReplyToMismatch = "reply_to_mismatch"
	AnomSilence         = "silence"
	AnomVolumeSpike     = "volume_spike"
)

// detector holds what one tick's checks need.
type detector struct {
	active      map[string]bool // account → history scan complete (per-message checks on)
	corrDomains map[string]bool // domains you correspond with (you have written to someone there)
	corrList    []string
	since       int64 // per-message checks only for mail at or after this time
}

// prepareDetector builds the tick's detector, or nil when detection is off.
func (sc *Scanner) prepareDetector(ctx context.Context, accounts []string) (*detector, error) {
	if !sc.cfg.AnomaliesOn() {
		return nil, nil
	}
	d := &detector{active: map[string]bool{}, corrDomains: map[string]bool{},
		since: sc.now().Add(-time.Duration(max(sc.cfg.AnomalyLookbackDays, 1)) * 24 * time.Hour).Unix()}
	for _, a := range accounts {
		var total, open int
		if err := sc.state.QueryRowContext(ctx, `SELECT count(*), count(*) - count(completed_at) FROM intel_scan WHERE account = ?`, a).
			Scan(&total, &open); err != nil {
			return nil, err
		}
		d.active[a] = total > 0 && open == 0
	}
	rows, err := sc.state.QueryContext(ctx, `SELECT DISTINCT domain FROM senders WHERE sent_count > 0 AND COALESCE(domain,'') <> ''`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var dom string
		if rows.Scan(&dom) == nil && !webmailDomains[dom] {
			d.corrDomains[dom] = true
			d.corrList = append(d.corrList, dom)
		}
	}
	rows.Close()
	for dom := range sc.ownDomains {
		d.corrDomains[dom] = true
	}
	return d, rows.Err()
}

// priorSender is the sender's profile before the current message is counted.
type priorSender struct {
	received, sent, dmarcPass, dkimPass int
	role                                string
}

// check runs the per-message checks for one new incoming message, inside the
// batch transaction, before the message is added to the profile. It returns
// the ids of new anomalies.
func (d *detector) check(ctx context.Context, tx *sql.Tx, cfg anomalyCfg, account, folder string, h Header, own ownSet) ([]int64, error) {
	if d == nil || !d.active[account] || h.Date.Unix() < d.since || own.has(h.From.Addr) {
		return nil, nil
	}
	var p priorSender
	err := tx.QueryRowContext(ctx, `SELECT message_count, sent_count, dmarc_pass, dkim_pass, COALESCE(role,'unknown') FROM senders WHERE address = ?`,
		h.From.Addr).Scan(&p.received, &p.sent, &p.dmarcPass, &p.dkimPass, &p.role)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if errors.Is(err, sql.ErrNoRows) {
		p.role = RoleUnknown
	}
	bulkRole := p.role == RoleNewsletter || p.role == RoleBot
	dom := domainOf(h.From.Addr)
	ref := strings.Trim(h.MessageID, "<>")
	if ref == "" {
		ref = fmt.Sprintf("noid:%s/%d", folder, h.UID)
	}
	var ids []int64
	add := func(typ, severity, desc string, details map[string]any) error {
		b, _ := json.Marshal(details)
		var id int64
		err := tx.QueryRowContext(ctx, `INSERT OR IGNORE INTO anomalies(account, sender, anomaly_type, description, severity, detected_at,
				folder, uid, message_ref, details) VALUES(?,?,?,?,?,unixepoch(),?,?,?,?) RETURNING id`,
			account, h.From.Addr, typ, desc, severity, folder, h.UID, ref, string(b)).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // already recorded for this message
		}
		if err == nil {
			ids = append(ids, id)
		}
		return err
	}

	if h.Conversation() && p.received == 0 && p.sent == 0 && !bulkRole {
		if err := add(AnomNewSender, "low", "first message from this address", map[string]any{"domain": dom}); err != nil {
			return nil, err
		}
	}
	if method, passes := authFailure(h, p); method != "" && passes >= cfg.authMinPasses {
		desc := fmt.Sprintf("%s failed; %d earlier messages from this sender passed", strings.ToUpper(method), passes)
		if err := add(AnomAuthFailure, "high", desc, map[string]any{"method": method, "earlier_passes": passes}); err != nil {
			return nil, err
		}
	}
	if dom != "" && !d.corrDomains[dom] && !webmailDomains[dom] {
		if like := d.lookalike(dom); like != "" {
			desc := fmt.Sprintf("domain %s resembles %s, which you correspond with", dom, like)
			if err := add(AnomLookalikeDomain, "high", desc, map[string]any{"domain": dom, "resembles": like}); err != nil {
				return nil, err
			}
		}
	}
	if rt := domainOf(h.ReplyTo.Addr); h.Conversation() && !bulkRole && p.received >= 3 && rt != "" && dom != "" && rt != dom {
		desc := fmt.Sprintf("Reply-To domain %s differs from the sender's domain %s", rt, dom)
		if err := add(AnomReplyToMismatch, "medium", desc, map[string]any{"reply_to_domain": rt, "from_domain": dom}); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

// authFailure reports the failed method (dmarc, or dkim when the message has
// no DMARC result) and how many earlier messages passed it.
func authFailure(h Header, p priorSender) (method string, passes int) {
	switch {
	case h.DMARC == "fail":
		return "dmarc", p.dmarcPass
	case h.DMARC == "" && h.DKIM == "fail":
		return "dkim", p.dkimPass
	}
	return "", 0
}

// lookalike returns a corresponded domain that dom imitates, or "".
func (d *detector) lookalike(dom string) string {
	if len(dom) < 6 {
		return "" // short domains are too close to everything
	}
	folded := foldConfusables(dom)
	for _, c := range d.corrList {
		if c == dom {
			continue
		}
		if foldConfusables(c) == folded {
			return c
		}
		limit := 1
		if len(c) >= 10 {
			limit = 2
		}
		if abs(len(c)-len(dom)) <= limit && editDistance(dom, c) <= limit {
			return c
		}
	}
	return ""
}

var confusables = strings.NewReplacer("rn", "m", "vv", "w", "0", "o", "1", "l", "i", "l")

func foldConfusables(s string) string { return confusables.Replace(strings.ToLower(s)) }

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// editDistance is the Levenshtein distance between two short strings.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

// anomalyCfg is the thresholds, copied from config once.
type anomalyCfg struct {
	authMinPasses, silenceMinMessages, silenceMinDays, spikeMin, spikeFactor int
}

func (sc *Scanner) anomalyCfg() anomalyCfg {
	c := sc.cfg
	return anomalyCfg{max(c.AnomalyAuthMinPasses, 1), max(c.AnomalySilenceMinMessages, 2), max(c.AnomalySilenceMinDays, 1),
		max(c.AnomalySpikeMin, 1), max(c.AnomalySpikeFactor, 1)}
}

// periodic runs the silence and volume-spike checks for accounts whose history
// scan is complete, and returns the new anomaly ids per account.
func (sc *Scanner) periodic(ctx context.Context, d *detector) (map[string][]int64, error) {
	out := map[string][]int64{}
	if d == nil {
		return out, nil
	}
	cfg := sc.anomalyCfg()
	now := sc.now().Unix()
	day := int64(24 * 3600)
	for account, on := range d.active {
		if !on {
			continue
		}
		// Silence: regular correspondents gone quiet, reported when the
		// silence crosses max(min days, 3x their usual gap) and for one more
		// min-days window after that, never for long-past silences.
		type quiet struct {
			id, received, first, lastIn int64
			addr                        string
		}
		var qs []quiet
		rows, err := sc.state.QueryContext(ctx, `SELECT s.id, s.address, s.message_count, COALESCE(s.first_seen,0), MAX(m.date)
			FROM senders s JOIN intel_messages m ON m.sender_id = s.id AND m.account = ? AND m.outgoing = 0
			WHERE s.role IN ('personal','colleague') AND s.message_count >= ? GROUP BY s.id`, account, cfg.silenceMinMessages)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var q quiet
			if err := rows.Scan(&q.id, &q.addr, &q.received, &q.first, &q.lastIn); err != nil {
				rows.Close()
				return out, err
			}
			qs = append(qs, q)
		}
		rows.Close()
		for _, q := range qs {
			gap := (q.lastIn - q.first) / max(q.received-1, 1)
			threshold := max(int64(cfg.silenceMinDays)*day, 3*gap)
			silent := now - q.lastIn
			if silent <= threshold {
				// Mail arrived again: an open silence finding is over.
				if _, err := sc.state.ExecContext(ctx, `UPDATE anomalies SET resolved = 1, resolved_at = unixepoch()
					WHERE account = ? AND sender = ? AND anomaly_type = ? AND resolved = 0`, account, q.addr, AnomSilence); err != nil {
					return out, err
				}
				continue
			}
			if silent > threshold+int64(cfg.silenceMinDays)*day {
				continue // a long-past silence is history, not news
			}
			desc := fmt.Sprintf("no mail for %d days; usually every %d days", silent/day, max(gap/day, 1))
			id, err := sc.insertOpen(ctx, account, q.addr, AnomSilence, "low", desc,
				map[string]any{"days_silent": silent / day, "usual_gap_days": gap / day, "messages": q.received})
			if err != nil {
				return out, err
			}
			if id != 0 {
				out[account] = append(out[account], id)
			}
		}

		// Volume spike in the last 24 hours.
		type burst struct {
			addr          string
			n, total, fst int64
		}
		var bs []burst
		rows, err = sc.state.QueryContext(ctx, `SELECT s.address, count(*), s.message_count, COALESCE(s.first_seen,0)
			FROM intel_messages m JOIN senders s ON s.id = m.sender_id
			WHERE m.account = ? AND m.outgoing = 0 AND m.date >= ? GROUP BY s.id HAVING count(*) > ?`, account, now-day, cfg.spikeMin)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var b burst
			if err := rows.Scan(&b.addr, &b.n, &b.total, &b.fst); err != nil {
				rows.Close()
				return out, err
			}
			bs = append(bs, b)
		}
		rows.Close()
		for _, b := range bs {
			days := max((now-b.fst)/day, 1)
			avg := float64(b.total-b.n) / float64(days)
			if float64(b.n) <= max(float64(cfg.spikeMin), float64(cfg.spikeFactor)*avg) {
				continue
			}
			desc := fmt.Sprintf("%d messages in 24 hours; usually %.1f a day", b.n, avg)
			id, err := sc.insertOpen(ctx, account, b.addr, AnomVolumeSpike, "medium", desc,
				map[string]any{"last_24h": b.n, "daily_average": avg})
			if err != nil {
				return out, err
			}
			if id != 0 {
				out[account] = append(out[account], id)
			}
		}
	}
	return out, nil
}

// insertOpen records a periodic finding unless one is already open for the
// same sender and type. It returns the new id, or 0.
func (sc *Scanner) insertOpen(ctx context.Context, account, sender, typ, severity, desc string, details map[string]any) (int64, error) {
	b, _ := json.Marshal(details)
	var id int64
	err := sc.state.QueryRowContext(ctx, `INSERT OR IGNORE INTO anomalies(account, sender, anomaly_type, description, severity, detected_at, details)
		VALUES(?,?,?,?,?,unixepoch(),?) RETURNING id`, account, sender, typ, desc, severity, string(b)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// scoreSenders sets each sender's anomaly_score to the severity-weighted
// count of their open anomalies (high 1, medium 0.5, low 0.2).
func (sc *Scanner) scoreSenders(ctx context.Context) error {
	_, err := sc.state.ExecContext(ctx, `UPDATE senders SET anomaly_score = COALESCE((SELECT sum(CASE a.severity WHEN 'high' THEN 1.0
			WHEN 'medium' THEN 0.5 ELSE 0.2 END) FROM anomalies a WHERE a.sender = senders.address AND a.resolved = 0), 0)
		WHERE anomaly_score > 0 OR address IN (SELECT sender FROM anomalies WHERE resolved = 0)`)
	return err
}

// publish sends anomaly.detected for new findings: identifiers, type and
// severity only (D16); receivers fetch details with their own token.
func (sc *Scanner) publish(ctx context.Context, account string, ids []int64) {
	if sc.bus == nil {
		return
	}
	for _, id := range ids {
		var typ, severity string
		if sc.state.QueryRowContext(ctx, `SELECT anomaly_type, severity FROM anomalies WHERE id = ?`, id).Scan(&typ, &severity) != nil {
			continue
		}
		sc.bus.Publish(bus.Event{Type: bus.EventAnomalyDetected, Account: account,
			Payload: map[string]any{"id": id, "type": typ, "severity": severity}})
	}
}
