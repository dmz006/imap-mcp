package intel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Roles a sender can have (senders.role).
const (
	RoleColleague  = "colleague"
	RoleVendor     = "vendor"
	RoleNewsletter = "newsletter"
	RoleBot        = "bot"
	RolePersonal   = "personal"
	RoleUnknown    = "unknown"
)

// senderStats is what role assignment looks at.
type senderStats struct {
	ID                                int64
	Address, Domain, Role, RoleSource string
	Received, Sent, List, Bulk, Auto  int
}

// webmailDomains are shared consumer domains: writing to someone at the same
// one does not make them a colleague.
var webmailDomains = map[string]bool{
	"gmail.com": true, "googlemail.com": true, "outlook.com": true, "hotmail.com": true, "live.com": true,
	"msn.com": true, "yahoo.com": true, "ymail.com": true, "icloud.com": true, "me.com": true, "mac.com": true,
	"aol.com": true, "proton.me": true, "protonmail.com": true, "pm.me": true, "gmx.com": true, "gmx.net": true,
	"mail.com": true, "zoho.com": true, "fastmail.com": true, "yandex.com": true, "hey.com": true,
}

// noReplyLocal are local parts that mark an automated sender.
var noReplyLocal = []string{"noreply", "no-reply", "no_reply", "donotreply", "do-not-reply", "do_not_reply",
	"mailer-daemon", "postmaster", "bounce", "bounces", "notifications", "notification", "alerts", "automated"}

// signalRole assigns a role from header signals, in the D20 order: list or
// bulk mail → newsletter; noreply or Auto-Submitted → bot; someone we have
// written to → colleague (same, non-webmail domain as one of our accounts) or
// personal. It returns RoleUnknown when no signal decides.
func signalRole(s senderStats, ownDomains map[string]bool) (role, source string) {
	if s.Received > 0 {
		if lists := s.List + s.Bulk; lists > 0 && lists*2 >= s.Received {
			return RoleNewsletter, "signal:list"
		}
		if s.Auto > 0 && s.Auto*2 >= s.Received {
			return RoleBot, "signal:auto-submitted"
		}
	}
	local := s.Address
	if i := strings.IndexByte(local, '@'); i >= 0 {
		local = local[:i]
	}
	for _, p := range noReplyLocal {
		if local == p || strings.HasPrefix(local, p+"+") || strings.HasPrefix(local, p+".") || strings.HasSuffix(local, "-"+p) {
			return RoleBot, "signal:noreply"
		}
	}
	if s.Sent > 0 {
		if ownDomains[s.Domain] && !webmailDomains[s.Domain] {
			return RoleColleague, "signal:sent-to"
		}
		return RolePersonal, "signal:sent-to"
	}
	return RoleUnknown, ""
}

// hallRole maps the enrichment classification of a sender's cached mail to a
// role.
func hallRole(hall string) string {
	switch hall {
	case "newsletter":
		return RoleNewsletter
	case "transactional":
		return RoleVendor
	case "notification", "alert":
		return RoleBot
	case "personal", "conversation":
		return RolePersonal
	}
	return RoleUnknown
}

// roleBatch is how many dirty senders are recomputed per query.
const roleBatch = 500

// recomputeRoles assigns roles to every sender marked dirty: signals first,
// then the majority classification tag of their cached mail. A role the
// model gave earlier is kept while signals and tags stay undecided.
func (sc *Scanner) recomputeRoles(ctx context.Context) (int, error) {
	total := 0
	for {
		rows, err := sc.state.QueryContext(ctx, `SELECT id, address, COALESCE(domain,''), COALESCE(role,'unknown'), COALESCE(role_source,''),
			message_count, sent_count, list_count, bulk_count, auto_count FROM senders WHERE dirty = 1 LIMIT ?`, roleBatch)
		if err != nil {
			return total, err
		}
		var batch []senderStats
		for rows.Next() {
			var s senderStats
			if err := rows.Scan(&s.ID, &s.Address, &s.Domain, &s.Role, &s.RoleSource, &s.Received, &s.Sent, &s.List, &s.Bulk, &s.Auto); err != nil {
				rows.Close()
				return total, err
			}
			batch = append(batch, s)
		}
		rows.Close()
		if len(batch) == 0 {
			return total, rows.Err()
		}
		halls := sc.majorityHalls(ctx, batch)
		tx, err := sc.state.BeginTx(ctx, nil)
		if err != nil {
			return total, err
		}
		for _, s := range batch {
			role, source := signalRole(s, sc.ownDomains)
			if role == RoleUnknown {
				if r := hallRole(halls[s.Address]); r != RoleUnknown {
					role, source = r, "hall"
				}
			}
			if role == RoleUnknown && s.RoleSource == "llm" {
				role, source = s.Role, s.RoleSource // keep the model's answer
			}
			if _, err := tx.ExecContext(ctx, `UPDATE senders SET role=?, role_source=NULLIF(?,''), dirty=0, updated_at=unixepoch() WHERE id=?`,
				role, source, s.ID); err != nil {
				tx.Rollback() //nolint:errcheck
				return total, err
			}
		}
		if err := tx.Commit(); err != nil {
			return total, err
		}
		total += len(batch)
	}
}

// majorityHalls returns, per address, the most common classification tag of
// its cached mail (ignoring unclassified mail). It is empty without a cache.
func (sc *Scanner) majorityHalls(ctx context.Context, batch []senderStats) map[string]string {
	out := map[string]string{}
	if sc.cache == nil || len(batch) == 0 {
		return out
	}
	args := make([]any, len(batch))
	marks := make([]string, len(batch))
	for i, s := range batch {
		args[i], marks[i] = s.Address, "?"
	}
	rows, err := sc.cache.QueryContext(ctx, `SELECT lower(from_addr), hall, count(*) AS n FROM messages
		WHERE lower(from_addr) IN (`+strings.Join(marks, ",")+`) AND hall IS NOT NULL AND hall NOT IN ('', 'unclassified')
		GROUP BY 1, 2 ORDER BY 1, n DESC`, args...)
	if err != nil {
		sc.log.Warn("intel: read cached classifications", "err", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var addr, hall string
		var n int
		if rows.Scan(&addr, &hall, &n) == nil {
			if _, ok := out[addr]; !ok { // first row per address has the highest count
				out[addr] = hall
			}
		}
	}
	return out
}

// ErrGated stops the model pass for this tick (the classifier is busy).
var ErrGated = errors.New("classifier gated")

// llmRetryAfter is how long before the model is asked again about a sender it
// could not place.
const llmRetryAfter = 7 * 24 * time.Hour

// modelRoles asks the classify model about senders that signals and tags left
// unknown and that have cached mail (their subjects are the only content
// sent, to the configured classify model). At most perTick calls; stops early
// when the classifier is gated.
func (sc *Scanner) modelRoles(ctx context.Context, perTick int) (int, error) {
	if sc.classify == nil || sc.cache == nil || perTick <= 0 {
		return 0, nil
	}
	// Start from senders that have cached mail (their subjects are what the
	// model judges by), busiest first; keep those still unknown and not asked
	// recently. Picking unknown senders by all-history volume instead finds
	// mostly old senders with nothing cached.
	rows, err := sc.cache.QueryContext(ctx, `SELECT lower(from_addr), count(*) AS n FROM messages
		GROUP BY 1 ORDER BY n DESC LIMIT 2000`)
	if err != nil {
		return 0, err
	}
	var cached []string
	for rows.Next() {
		var a string
		var n int
		if rows.Scan(&a, &n) == nil {
			cached = append(cached, a)
		}
	}
	rows.Close()
	type cand struct {
		id         int64
		addr, name string
	}
	retry := sc.now().Add(-llmRetryAfter).Unix()
	var cands []cand
	for _, a := range cached {
		if len(cands) >= perTick*2 {
			break
		}
		var c cand
		err := sc.state.QueryRowContext(ctx, `SELECT id, address, COALESCE(name,'') FROM senders
			WHERE address = ? AND role = 'unknown' AND (role_checked_at IS NULL OR role_checked_at < ?)`, a, retry).Scan(&c.id, &c.addr, &c.name)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return 0, err
		}
		cands = append(cands, c)
	}

	done := 0
	for _, c := range cands {
		if done >= perTick || ctx.Err() != nil {
			break
		}
		subjects := sc.recentSubjects(ctx, c.addr, 5)
		if len(subjects) == 0 {
			continue // nothing cached to judge by
		}
		answer, err := sc.classify(ctx, rolePrompt(c.addr, c.name, subjects))
		if errors.Is(err, ErrGated) {
			break
		}
		role, source := RoleUnknown, "llm:none"
		if err == nil {
			if r := parseRole(answer); r != RoleUnknown {
				role, source = r, "llm"
			}
		} else {
			sc.log.Debug("intel: role model call failed", "err", err)
			source = "llm:error"
		}
		if _, err := sc.state.ExecContext(ctx, `UPDATE senders SET role=?, role_source=?, role_checked_at=?, updated_at=unixepoch() WHERE id=?`,
			role, source, sc.now().Unix(), c.id); err != nil {
			return done, err
		}
		done++
	}
	return done, nil
}

func (sc *Scanner) recentSubjects(ctx context.Context, addr string, n int) []string {
	rows, err := sc.cache.QueryContext(ctx, `SELECT COALESCE(subject,'') FROM messages WHERE lower(from_addr) = ?
		ORDER BY COALESCE(internal_date, date) DESC LIMIT ?`, addr, n)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if rows.Scan(&s) == nil && strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}

// rolePrompt asks for a role as JSON. Only the sender and a few subjects are
// included, never bodies.
func rolePrompt(addr, name string, subjects []string) string {
	var b strings.Builder
	b.WriteString("Classify this email sender's relationship to the mailbox owner.\n")
	b.WriteString(`Reply with JSON only: {"role": "<colleague|vendor|newsletter|bot|personal>"}` + "\n")
	b.WriteString("colleague = works with the owner; vendor = a company the owner buys from or uses; ")
	b.WriteString("newsletter = mailing list or marketing; bot = automated notifications; personal = friends or family.\n\n")
	b.WriteString("Sender: ")
	if name != "" {
		b.WriteString(name + " ")
	}
	b.WriteString("<" + addr + ">\nRecent subjects:\n")
	for _, s := range subjects {
		if len(s) > 200 {
			s = strings.ToValidUTF8(s[:200], "")
		}
		b.WriteString("- " + strings.ReplaceAll(s, "\n", " ") + "\n")
	}
	return b.String()
}

// parseRole reads {"role": "..."} from a model answer, tolerating text or a
// <think> block around it. Anything else is RoleUnknown.
func parseRole(answer string) string {
	if i := strings.LastIndex(answer, "</think>"); i >= 0 {
		answer = answer[i+len("</think>"):]
	}
	start, end := strings.Index(answer, "{"), strings.LastIndex(answer, "}")
	if start < 0 || end <= start {
		return RoleUnknown
	}
	var v struct {
		Role string `json:"role"`
	}
	if json.Unmarshal([]byte(answer[start:end+1]), &v) != nil {
		return RoleUnknown
	}
	switch r := strings.ToLower(strings.TrimSpace(v.Role)); r {
	case RoleColleague, RoleVendor, RoleNewsletter, RoleBot, RolePersonal:
		return r
	}
	return RoleUnknown
}
