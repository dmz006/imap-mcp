package service

import (
	"context"
	"fmt"
	"log/slog"
	"net/mail"
	"slices"
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/unsubscribe"
)

// unsubConfirmWindow is how long a dry run authorises a confirmed call (D39).
const unsubConfirmWindow = time.Hour

// unsubArchiveCap bounds how many messages one archive moves.
const unsubArchiveCap = 5000

// UnsubscribeParams selects the sender to unsubscribe from: Sender (an
// address) or the UID of one of their messages in Folder.
type UnsubscribeParams struct {
	Account   string
	Folder    string // default INBOX
	Sender    string
	UID       uint32
	Confirm   bool   // false = dry run (D39)
	ArchiveTo string // move the sender's mail in Folder here after success (D57)
	// CanSend says the caller may send mail; a mailto unsubscribe needs it
	// (D40). The MCP and REST layers set it from the token's scopes.
	CanSend bool
}

// UnsubscribeResult is a dry run's plan, or what a confirmed call did.
type UnsubscribeResult struct {
	Account  string                     `json:"account"`
	Sender   string                     `json:"sender"`
	DryRun   bool                       `json:"dry_run"`
	Plan     unsubscribe.Plan           `json:"plan"`
	Status   string                     `json:"status"` // dry_run | done | failed | sent | not_possible
	HTTP     *unsubscribe.Result        `json:"http,omitempty"`
	Archived int                        `json:"archived,omitempty"`
	Archive  string                     `json:"archive_to,omitempty"`
	Messages int                        `json:"messages_from_sender"`
	Next     string                     `json:"next,omitempty"`
	Mailto   *unsubscribe.MailtoMessage `json:"mailto_sent,omitempty"`
}

// unsubOptions lets tests replace DNS and the HTTP client.
var unsubOptions = unsubscribe.Options{}
var unsubClient = func(cfg *config.Config) *unsubscribe.Client {
	return unsubscribe.NewClient(config.Version, cfg.Unsub.TimeoutOrDefault(), false)
}

// Unsubscribe unsubscribes from a sender the RFC 8058 one-click way, or by
// the sender's mailto address, never by following links (AGENT.md Q4,
// D39–D41, D57). Without Confirm it is a dry run: the method that would be
// used and why. A confirmed call needs a dry run for the same sender in the
// last hour.
func (s *Service) Unsubscribe(ctx context.Context, p UnsubscribeParams) (UnsubscribeResult, error) {
	if s.db == nil || s.db.StateSQL() == nil {
		return UnsubscribeResult{}, unavailable("the state database is not open in this mode")
	}
	if p.Sender == "" && p.UID == 0 {
		return UnsubscribeResult{}, invalid("sender or uid is required")
	}
	if p.Folder == "" {
		p.Folder = "INBOX"
	}
	if p.Sender != "" {
		a, err := mail.ParseAddress(p.Sender)
		if err != nil {
			return UnsubscribeResult{}, invalid("sender %q is not an email address", p.Sender)
		}
		p.Sender = strings.ToLower(a.Address)
	}
	c, err := s.conn(p.Account)
	if err != nil {
		return UnsubscribeResult{}, err
	}
	account := c.Account()

	uid, sender, uids, err := s.unsubFind(account, p)
	if err != nil {
		return UnsubscribeResult{}, err
	}
	raws, _, err := s.fetchRaw([]ExportRef{{Account: account, Folder: p.Folder, UID: uid}}, 25<<20, false)
	if err != nil {
		return UnsubscribeResult{}, err
	}
	plan := unsubscribe.Analyze(raws[0].data, unsubOptions)
	res := UnsubscribeResult{Account: account, Sender: sender, DryRun: !p.Confirm, Plan: plan, Messages: len(uids)}
	st := s.db.StateSQL()
	now := time.Now()

	if !p.Confirm {
		res.Status = "dry_run"
		if plan.Method == unsubscribe.MethodNone {
			res.Status = "not_possible"
		}
		switch plan.Method {
		case unsubscribe.MethodOneClick:
			res.Next = "Call again with confirm: true to send the one-click unsubscribe."
		case unsubscribe.MethodMailto:
			res.Next = "Call again with confirm: true to send this unsubscribe email; it needs the send scope too."
		default:
			res.Next = "Not possible here. A rule can trash or move this sender's mail instead (create_rule)."
		}
		if _, err := st.ExecContext(ctx, `INSERT INTO unsubscribes(account, sender, method, host, status, created_at) VALUES(?,?,?,?,?,?)`,
			account, sender, plan.Method, plan.Host, "dry_run", now.Unix()); err != nil {
			return res, err
		}
		return res, nil
	}

	var dry int
	if err := st.QueryRowContext(ctx, `SELECT count(*) FROM unsubscribes WHERE account = ? AND sender = ? AND status = 'dry_run' AND created_at >= ?`,
		account, sender, now.Add(-unsubConfirmWindow).Unix()).Scan(&dry); err != nil {
		return res, err
	}
	if dry == 0 {
		return res, invalid("run a dry run for %s first (confirm: false), then confirm within an hour", sender)
	}

	record := func(status string, code int, errText string, archived int) error {
		_, err := st.ExecContext(ctx, `INSERT INTO unsubscribes(account, sender, method, host, status, status_code, error, archived, created_at)
			VALUES(?,?,?,?,?,?,?,?,?)`, account, sender, plan.Method, plan.Host, status, code, errText, archived, now.Unix())
		return err
	}
	switch plan.Method {
	case unsubscribe.MethodOneClick:
		r := unsubClient(s.cfg).OneClick(ctx, plan.URL)
		res.HTTP = &r
		res.Status = "failed"
		if r.OK {
			res.Status = "done"
		}
	case unsubscribe.MethodMailto:
		if !p.CanSend {
			return res, &Error{Kind: KindForbidden, Msg: "a mailto unsubscribe sends an email: the token needs the send scope too (D40)"}
		}
		if _, err := s.Send(ctx, SendParams{Account: account, To: plan.Mailto.To, Subject: plan.Mailto.Subject, Body: plan.Mailto.Body}); err != nil {
			_ = record("failed", 0, "mailto send failed", 0)
			return res, err
		}
		res.Status, res.Mailto = "sent", plan.Mailto
	default:
		res.Status = "not_possible"
		res.Next = "A rule can trash or move this sender's mail instead (create_rule)."
		return res, record("not_possible", 0, plan.Reason, 0)
	}

	if (res.Status == "done" || res.Status == "sent") && p.ArchiveTo != "" {
		n, err := s.unsubArchive(account, p.Folder, p.ArchiveTo, uids)
		if err != nil {
			_ = record(res.Status, statusCode(res.HTTP), "archive failed", 0)
			return res, err
		}
		res.Archived, res.Archive = n, p.ArchiveTo
	}
	errText := ""
	if res.HTTP != nil {
		errText = res.HTTP.Error
	}
	if err := record(res.Status, statusCode(res.HTTP), errText, res.Archived); err != nil {
		return res, err
	}
	// The sender and URL stay out of the log; method and status make it auditable.
	slog.Info("unsubscribe", "account", account, "method", plan.Method, "status", res.Status, "http_status", statusCode(res.HTTP))
	if b := s.pool.Bus(); b != nil {
		b.Publish(bus.Event{Type: bus.EventUnsubscribeDone, Account: account, Payload: map[string]any{
			"account": account, "sender": sender, "method": plan.Method, "status": res.Status,
			"status_code": statusCode(res.HTTP), "archived": res.Archived}})
	}
	if res.Status == "done" || res.Status == "sent" {
		res.Next = fmt.Sprintf("Mail from this sender more than %d day(s) from now is reported in the digest (D56).", s.cfg.Unsub.GraceDaysOrDefault())
	}
	return res, nil
}

func statusCode(r *unsubscribe.Result) int {
	if r == nil {
		return 0
	}
	return r.Status
}

// unsubFind returns the message to read the headers from (the sender's
// newest in the folder, or p.UID), the sender, and all the sender's UIDs in
// the folder. IMAP SEARCH FROM matches substrings and display names, so
// envelopes are checked for the exact address.
func (s *Service) unsubFind(account string, p UnsubscribeParams) (uint32, string, []imaplib.UID, error) {
	sender := p.Sender
	var uids []imaplib.UID
	_, err := s.withFolder(account, p.Folder, func(client *imapclient.Client) (int64, error) {
		if p.UID != 0 {
			msgs, err := client.Fetch(imaplib.UIDSetNum(imaplib.UID(p.UID)), &imaplib.FetchOptions{UID: true, Envelope: true}).Collect()
			if err != nil {
				return 0, upstream("fetch", err)
			}
			if len(msgs) == 0 || msgs[0].Envelope == nil || len(msgs[0].Envelope.From) == 0 {
				return 0, notFound("uid %d not found in %s", p.UID, p.Folder)
			}
			from := strings.ToLower(msgs[0].Envelope.From[0].Addr())
			if sender != "" && sender != from {
				return 0, invalid("uid %d is from %s, not %s", p.UID, from, sender)
			}
			sender = from
		}
		sd, err := client.UIDSearch(&imaplib.SearchCriteria{Header: []imaplib.SearchCriteriaHeaderField{{Key: "From", Value: sender}}}, nil).Wait()
		if err != nil {
			return 0, upstream("search", err)
		}
		found := sd.AllUIDs()
		if len(found) > unsubArchiveCap {
			found = found[len(found)-unsubArchiveCap:]
		}
		if len(found) == 0 {
			return 0, nil
		}
		msgs, err := client.Fetch(imaplib.UIDSetNum(found...), &imaplib.FetchOptions{UID: true, Envelope: true}).Collect()
		if err != nil {
			return 0, upstream("fetch envelopes", err)
		}
		for _, m := range msgs {
			if m.Envelope != nil && len(m.Envelope.From) > 0 && strings.EqualFold(m.Envelope.From[0].Addr(), sender) {
				uids = append(uids, m.UID)
			}
		}
		return 0, nil
	})
	if err != nil {
		return 0, "", nil, err
	}
	slices.Sort(uids)
	uid := p.UID
	if uid == 0 {
		if len(uids) == 0 {
			return 0, "", nil, notFound("no mail from %s in %s", sender, p.Folder)
		}
		uid = uint32(uids[len(uids)-1])
	}
	return uid, sender, uids, nil
}

// unsubArchive moves the sender's messages out of folder.
func (s *Service) unsubArchive(account, folder, dest string, uids []imaplib.UID) (int, error) {
	if len(uids) == 0 {
		return 0, nil
	}
	c, err := s.conn(account)
	if err != nil {
		return 0, err
	}
	c.Lock()
	defer c.Unlock()
	client := c.Client()
	if _, err := client.Select(folder, nil).Wait(); err != nil {
		return 0, notFound("select %s: %v", folder, err)
	}
	if err := MoveUIDs(client, imaplib.UIDSetNum(uids...), dest); err != nil {
		return 0, upstream("move to "+dest, err)
	}
	return len(uids), nil
}

// UnsubscribeRecord is one row of the unsubscribe log.
type UnsubscribeRecord struct {
	ID         int64  `json:"id"`
	Account    string `json:"account"`
	Sender     string `json:"sender"`
	Method     string `json:"method"`
	Host       string `json:"host,omitempty"`
	Status     string `json:"status"`
	StatusCode int    `json:"status_code,omitempty"`
	Error      string `json:"error,omitempty"`
	Archived   int    `json:"archived,omitempty"`
	At         string `json:"at"`
}

// UnsubscribeLog lists unsubscribes, newest first (dry runs only with
// withDryRuns), and senders still mailing after the grace period (D56).
type UnsubscribeLog struct {
	Count   int                 `json:"count"`
	Items   []UnsubscribeRecord `json:"items"`
	Ignored []IgnoredUnsub      `json:"still_mailing"`
}

// ListUnsubscribes returns the unsubscribe log.
func (s *Service) ListUnsubscribes(ctx context.Context, account string, withDryRuns bool, limit int) (UnsubscribeLog, error) {
	out := UnsubscribeLog{Items: []UnsubscribeRecord{}, Ignored: []IgnoredUnsub{}}
	if s.db == nil || s.db.StateSQL() == nil {
		return out, unavailable("the state database is not open in this mode")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	q := `SELECT id, account, sender, method, COALESCE(host,''), status, COALESCE(status_code,0), COALESCE(error,''), archived, created_at
		FROM unsubscribes WHERE (? = '' OR account = ?)`
	if !withDryRuns {
		q += ` AND status <> 'dry_run'`
	}
	rows, err := s.db.StateSQL().QueryContext(ctx, q+` ORDER BY id DESC LIMIT ?`, account, account, limit)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var r UnsubscribeRecord
		var at int64
		if err := rows.Scan(&r.ID, &r.Account, &r.Sender, &r.Method, &r.Host, &r.Status, &r.StatusCode, &r.Error, &r.Archived, &at); err != nil {
			rows.Close()
			return out, err
		}
		r.At = time.Unix(at, 0).UTC().Format(time.RFC3339)
		out.Items = append(out.Items, r)
	}
	rows.Close()
	out.Count = len(out.Items)
	ign, err := s.ignoredUnsubscribes(ctx, account, false)
	if err != nil {
		return out, err
	}
	out.Ignored = ign
	return out, nil
}

// IgnoredUnsub is a sender still mailing after an unsubscribe (D56).
type IgnoredUnsub struct {
	ID             int64  `json:"id"` // the unsubscribe's log id
	Account        string `json:"account"`
	Sender         string `json:"sender"`
	UnsubscribedAt string `json:"unsubscribed_at"`
	Since          int    `json:"messages_since"`
	Rule           string `json:"offer"`
	reported       bool
}

// ignoredUnsubscribes finds senders with mail indexed more than the grace
// period after their latest successful unsubscribe. onlyNew skips those
// already reported in a digest.
func (s *Service) ignoredUnsubscribes(ctx context.Context, account string, onlyNew bool) ([]IgnoredUnsub, error) {
	out := []IgnoredUnsub{}
	if !s.cfg.Intel.On() {
		return out, nil // mail arrival is read from the history scan
	}
	st := s.db.StateSQL()
	grace := int64(s.cfg.Unsub.GraceDaysOrDefault()) * 86400
	rows, err := st.QueryContext(ctx, `SELECT u.id, u.account, u.sender, u.created_at, u.ignored_reported FROM unsubscribes u
		WHERE u.status IN ('done','sent') AND (? = '' OR u.account = ?)
		AND u.id = (SELECT max(id) FROM unsubscribes x WHERE x.account = u.account AND x.sender = u.sender AND x.status IN ('done','sent'))
		ORDER BY u.id`, account, account)
	if err != nil {
		return out, err
	}
	var cands []IgnoredUnsub
	var at []int64
	for rows.Next() {
		var r IgnoredUnsub
		var t, rep int64
		if err := rows.Scan(&r.ID, &r.Account, &r.Sender, &t, &rep); err != nil {
			rows.Close()
			return out, err
		}
		r.reported = rep == 1
		r.UnsubscribedAt = time.Unix(t, 0).UTC().Format(time.RFC3339)
		cands = append(cands, r)
		at = append(at, t)
	}
	rows.Close()
	for i, r := range cands {
		if onlyNew && r.reported {
			continue
		}
		var n int
		if err := st.QueryRowContext(ctx, `SELECT count(*) FROM intel_messages m JOIN senders s ON s.id = m.sender_id
			WHERE m.account = ? AND s.address = ? AND m.outgoing = 0 AND m.date > ?`, r.Account, r.Sender, at[i]+grace).Scan(&n); err != nil {
			return out, err
		}
		if n == 0 {
			continue
		}
		r.Since = n
		r.Rule = fmt.Sprintf(`create_rule {name: "unsubscribe ignored: %s", account: %q, from: %q, action: "trash", active: false}`, r.Sender, r.Account, r.Sender)
		out = append(out, r)
	}
	return out, nil
}

// markIgnoredReported records that the digest listed these senders.
func (s *Service) markIgnoredReported(ctx context.Context, items []IgnoredUnsub) error {
	for _, r := range items {
		if _, err := s.db.StateSQL().ExecContext(ctx, `UPDATE unsubscribes SET ignored_reported = 1 WHERE id = ?`, r.ID); err != nil {
			return err
		}
	}
	return nil
}
