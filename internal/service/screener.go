package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/mail"
	"slices"
	"strconv"
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/db"
)

// Screener actions and destinations (AGENT.md Q5).
const (
	ScreenApprove = "approve"
	ScreenBlock   = "block"
	DestInbox     = "inbox"
	DestFeed      = "feed"
	DestReceipts  = "receipts"
)

// HeldSender is one sender with mail waiting in a holding folder.
type HeldSender struct {
	Number   int      `json:"number"` // position in this list (and in the digest)
	Account  string   `json:"account"`
	Sender   string   `json:"sender"`
	Messages int      `json:"messages"`
	Folder   string   `json:"folder"`
	Reasons  string   `json:"reasons,omitempty"`
	Subjects []string `json:"subjects,omitempty"`
	HeldAt   string   `json:"last_held_at"`
}

// HeldList is held_senders' result.
type HeldList struct {
	Count   int          `json:"count"`
	Senders []HeldSender `json:"senders"`
}

// HeldSenders lists senders with undecided held mail, longest-held first.
func (s *Service) HeldSenders(ctx context.Context, account string) (HeldList, error) {
	out := HeldList{Senders: []HeldSender{}}
	if s.db == nil || s.db.StateSQL() == nil {
		return out, unavailable("the state database is not open in this mode")
	}
	acct := ""
	if account != "" {
		acct = s.accountName(account)
	}
	rows, err := s.db.StateSQL().QueryContext(ctx, `SELECT account, COALESCE(sender,''), COALESCE(folder,''), COALESCE(reasons,''),
			COALESCE(subject,''), held_at
		FROM held_messages WHERE released_at IS NULL AND decision IS NULL AND (? = '' OR account = ?) AND COALESCE(sender,'') <> ''
		ORDER BY account, held_at`, acct, acct)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	idx := map[string]int{}
	for rows.Next() {
		var a, sender, folder, reasons, subject string
		var at int64
		if err := rows.Scan(&a, &sender, &folder, &reasons, &subject, &at); err != nil {
			return out, err
		}
		k := a + "\x00" + strings.ToLower(sender)
		i, ok := idx[k]
		if !ok {
			i = len(out.Senders)
			idx[k] = i
			out.Senders = append(out.Senders, HeldSender{Account: a, Sender: strings.ToLower(sender), Folder: folder, Reasons: reasons})
		}
		h := &out.Senders[i]
		h.Messages++
		h.HeldAt = time.Unix(at, 0).UTC().Format(time.RFC3339)
		if len(h.Subjects) < 3 && subject != "" {
			h.Subjects = append(h.Subjects, subject)
		}
	}
	n := map[string]int{}
	for i := range out.Senders {
		n[out.Senders[i].Account]++
		out.Senders[i].Number = n[out.Senders[i].Account]
	}
	out.Count = len(out.Senders)
	return out, rows.Err()
}

// ScreenDecision is one decision about a held sender.
type ScreenDecision struct {
	Sender string `json:"sender"`
	Action string `json:"action"`         // approve | block
	Dest   string `json:"dest,omitempty"` // approve: inbox (default) | feed | receipts
}

// ScreenParams are screen_senders' arguments. Without Confirm it is a dry
// run (D62).
type ScreenParams struct {
	Account   string           `json:"account"`
	Decisions []ScreenDecision `json:"decisions"`
	Confirm   bool             `json:"confirm,omitempty"`
	via       string
}

// ScreenRule is the rule a decision creates.
type ScreenRule struct {
	ID      int64  `json:"id,omitempty"`
	From    string `json:"from"`
	Action  string `json:"action"` // move | trash
	Dest    string `json:"dest,omitempty"`
	Active  bool   `json:"active"`
	Matches int    `json:"matches_in_inbox"` // what it would match in INBOX now (D43 preview)
}

// ScreenOutcome is what one decision did, or would do.
type ScreenOutcome struct {
	Sender   string      `json:"sender"`
	Action   string      `json:"action"`
	Dest     string      `json:"dest,omitempty"`
	Folder   string      `json:"move_to,omitempty"` // where the held mail goes
	Messages int         `json:"held_messages"`
	Moved    int         `json:"moved,omitempty"`
	Trusted  bool        `json:"trusted,omitempty"`
	Rule     *ScreenRule `json:"rule,omitempty"`
	Error    string      `json:"error,omitempty"`
}

// ScreenResult is screen_senders' result.
type ScreenResult struct {
	Account  string          `json:"account"`
	DryRun   bool            `json:"dry_run"`
	Outcomes []ScreenOutcome `json:"decisions"`
	// Run is the exact call that applies this preview (D62).
	Run string `json:"run,omitempty"`
}

// Screen applies (or previews) decisions about held senders (Q5): approve
// trusts the sender and moves its held mail to the inbox, feed or receipts
// folder (with a routing rule for feed and receipts); block trashes its held
// mail and creates a trash rule for its domain, or only the address for
// freemail or a correspondent's domain (D43). Rules start inactive unless
// rules.screener.rules_active (D61).
func (s *Service) Screen(ctx context.Context, p ScreenParams) (ScreenResult, error) {
	if s.db == nil || s.db.StateSQL() == nil {
		return ScreenResult{}, unavailable("the state database is not open in this mode")
	}
	if len(p.Decisions) == 0 {
		return ScreenResult{}, invalid("decisions are required: [{sender, action: approve|block, dest: inbox|feed|receipts}]")
	}
	c, err := s.conn(p.Account)
	if err != nil {
		return ScreenResult{}, err
	}
	account := c.Account()
	if p.via == "" {
		p.via = "mcp"
	}
	res := ScreenResult{Account: account, DryRun: !p.Confirm}
	for i := range p.Decisions {
		d := &p.Decisions[i]
		d.Action = strings.ToLower(strings.TrimSpace(d.Action))
		d.Dest = strings.ToLower(strings.TrimSpace(d.Dest))
		if a, err := mail.ParseAddress(d.Sender); err == nil {
			d.Sender = strings.ToLower(a.Address)
		} else {
			d.Sender = strings.ToLower(strings.TrimSpace(d.Sender))
		}
		switch d.Action {
		case ScreenApprove:
			if d.Dest == "" {
				d.Dest = DestInbox
			}
			if d.Dest != DestInbox && d.Dest != DestFeed && d.Dest != DestReceipts {
				return res, invalid("dest must be inbox, feed or receipts (got %q)", d.Dest)
			}
		case ScreenBlock:
			d.Dest = ""
		default:
			return res, invalid("action must be approve or block (got %q)", d.Action)
		}
	}
	sc := s.cfg.Rules.Screener
	trash := ""
	if p.Confirm || slices.ContainsFunc(p.Decisions, func(d ScreenDecision) bool { return d.Action == ScreenBlock }) {
		if trash, err = s.trashFolder(account); err != nil {
			return res, err
		}
	}
	var approved, blocked, moved int
	for _, d := range p.Decisions {
		o := ScreenOutcome{Sender: d.Sender, Action: d.Action, Dest: d.Dest}
		held, err := s.heldOf(ctx, account, d.Sender)
		if err != nil {
			return res, err
		}
		o.Messages = len(held)
		if len(held) == 0 {
			o.Error = "no undecided held mail from this sender"
			res.Outcomes = append(res.Outcomes, o)
			continue
		}
		switch d.Action {
		case ScreenApprove:
			switch d.Dest {
			case DestInbox:
				o.Folder = "INBOX"
			case DestFeed:
				o.Folder = sc.Feed()
			case DestReceipts:
				o.Folder = sc.Receipts()
			}
			o.Trusted = true
			if d.Dest != DestInbox {
				o.Rule = &ScreenRule{From: d.Sender, Action: "move", Dest: o.Folder, Active: sc.RulesActive}
			}
		case ScreenBlock:
			o.Folder = trash
			o.Rule = &ScreenRule{From: s.blockTarget(ctx, d.Sender), Action: "trash", Active: sc.RulesActive}
		}
		if o.Rule != nil {
			o.Rule.Matches, _ = s.inboxMatches(account, o.Rule.From)
		}
		if p.Confirm {
			if err := s.applyScreen(ctx, account, d, &o, held, p.via); err != nil {
				o.Error = err.Error()
			} else if d.Action == ScreenApprove {
				approved++
			} else {
				blocked++
			}
			moved += o.Moved
		}
		res.Outcomes = append(res.Outcomes, o)
	}
	if !p.Confirm {
		live := ScreenParams{Account: account, Decisions: p.Decisions, Confirm: true}
		b, _ := json.Marshal(live)
		res.Run = "screen_senders " + string(b)
		return res, nil
	}
	slog.Info("screener", "account", account, "via", p.via, "approved", approved, "blocked", blocked, "moved", moved)
	if b := s.pool.Bus(); b != nil && approved+blocked > 0 {
		b.Publish(bus.Event{Type: bus.EventScreenerDecided, Account: account, Payload: map[string]any{
			"account": account, "approved": approved, "blocked": blocked, "moved": moved, "via": p.via}})
	}
	return res, nil
}

type heldRef struct {
	hash        int64
	ref, folder string
}

func (s *Service) heldOf(ctx context.Context, account, sender string) ([]heldRef, error) {
	rows, err := s.db.StateSQL().QueryContext(ctx, `SELECT msg_hash, COALESCE(message_ref,''), COALESCE(folder,'') FROM held_messages
		WHERE account = ? AND lower(COALESCE(sender,'')) = ? AND released_at IS NULL AND decision IS NULL`, account, sender)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []heldRef
	for rows.Next() {
		var h heldRef
		if err := rows.Scan(&h.hash, &h.ref, &h.folder); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// blockTarget is what a block rule matches (D43): the domain, unless it is
// freemail or the owner has a correspondent there, then the address.
func (s *Service) blockTarget(ctx context.Context, sender string) string {
	domain := domainOf(sender)
	if domain == "" || freemailDomains[domain] {
		return sender
	}
	var n int
	_ = s.db.StateSQL().QueryRowContext(ctx, `SELECT count(*) FROM senders WHERE domain = ? AND address <> ?
		AND (sent_count > 0 OR reply_count > 0 OR COALESCE(trusted,0) = 1 OR COALESCE(role,'') = 'self')`, domain, sender).Scan(&n)
	if n > 0 || s.me(ctx).has(sender) {
		return sender
	}
	return domain
}

// inboxMatches counts INBOX messages whose From matches target (the rule's
// own matching, IMAP SEARCH FROM).
func (s *Service) inboxMatches(account, target string) (int, error) {
	n, err := s.withFolder(account, "INBOX", func(client *imapclient.Client) (int64, error) {
		sd, err := client.UIDSearch(&imaplib.SearchCriteria{Header: []imaplib.SearchCriteriaHeaderField{{Key: "From", Value: target}}}, nil).Wait()
		if err != nil {
			return 0, upstream("search", err)
		}
		return int64(len(sd.AllUIDs())), nil
	})
	return int(n), err
}

func (s *Service) trashFolder(account string) (string, error) {
	c, err := s.conn(account)
	if err != nil {
		return "", err
	}
	c.Lock()
	defer c.Unlock()
	t, err := ResolveTrash(c.Client())
	if err != nil {
		return "", upstream("find Trash", err)
	}
	return t, nil
}

// applyScreen carries out one decision: move the held mail, update the
// hold records and trust, and create the rule.
func (s *Service) applyScreen(ctx context.Context, account string, d ScreenDecision, o *ScreenOutcome, held []heldRef, via string) error {
	if d.Action == ScreenApprove && d.Dest != DestInbox {
		if err := s.ensureFolder(account, o.Folder); err != nil {
			return err
		}
	}
	byFolder := map[string][]heldRef{}
	for _, h := range held {
		byFolder[h.folder] = append(byFolder[h.folder], h)
	}
	for folder, hs := range byFolder {
		if folder == "" || strings.EqualFold(folder, o.Folder) {
			continue
		}
		n, err := s.moveByMessageID(account, folder, o.Folder, hs)
		if err != nil {
			return err
		}
		o.Moved += n
	}
	st := s.db.StateSQL()
	now := time.Now().Unix()
	decision := ScreenBlock
	if d.Action == ScreenApprove {
		decision = ScreenApprove + ":" + d.Dest
	}
	for _, h := range held {
		q := `UPDATE held_messages SET decision = ? WHERE account = ? AND msg_hash = ?`
		if d.Action == ScreenApprove {
			q = `UPDATE held_messages SET decision = ?, released_at = ` + strconv.FormatInt(now, 10) + ` WHERE account = ? AND msg_hash = ?`
		}
		if _, err := st.ExecContext(ctx, q, decision, account, h.hash); err != nil {
			return err
		}
	}
	if d.Action == ScreenApprove {
		if _, err := st.ExecContext(ctx, `INSERT INTO senders(address, domain, first_seen, trusted) VALUES(?,?,?,1)
			ON CONFLICT(address) DO UPDATE SET trusted = 1`, d.Sender, domainOf(d.Sender), now); err != nil {
			return err
		}
	}
	var ruleID int64
	if o.Rule != nil {
		r := &db.Rule{
			Name:        fmt.Sprintf("screener: %s %s", d.Action, o.Rule.From),
			Description: "Created by the screener (Q5)",
			Conditions:  db.RuleConditions{Account: account, From: o.Rule.From},
			Active:      o.Rule.Active,
		}
		if o.Rule.Action == "trash" {
			r.Actions = []db.RuleAction{{Type: "trash"}}
		} else {
			r.Actions = []db.RuleAction{{Type: "move", Dest: o.Rule.Dest}}
		}
		id, err := s.CreateRule(ctx, r)
		if err != nil {
			return err
		}
		o.Rule.ID, ruleID = id, id
	}
	target := ""
	if o.Rule != nil {
		target = o.Rule.From
	}
	_, err := st.ExecContext(ctx, `INSERT INTO screen_decisions(account, sender, action, dest, target, rule_id, moved, via, created_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, account, d.Sender, d.Action, d.Dest, target, ruleID, o.Moved, via, now)
	return err
}

// moveByMessageID moves held messages found by Message-ID from folder to dest.
func (s *Service) moveByMessageID(account, folder, dest string, hs []heldRef) (int, error) {
	c, err := s.conn(account)
	if err != nil {
		return 0, err
	}
	c.Lock()
	defer c.Unlock()
	client := c.Client()
	if _, err := client.Select(folder, nil).Wait(); err != nil {
		return 0, nil // the holding folder is gone: nothing to move
	}
	var uids []imaplib.UID
	for _, h := range hs {
		if h.ref == "" {
			continue
		}
		id := h.ref
		if !strings.HasPrefix(id, "<") {
			id = "<" + id + ">"
		}
		sd, err := client.UIDSearch(&imaplib.SearchCriteria{Header: []imaplib.SearchCriteriaHeaderField{{Key: "Message-ID", Value: id}}}, nil).Wait()
		if err != nil {
			return 0, upstream("search", err)
		}
		uids = append(uids, sd.AllUIDs()...)
	}
	if len(uids) == 0 {
		return 0, nil
	}
	if err := MoveUIDs(client, imaplib.UIDSetNum(uids...), dest); err != nil {
		return 0, upstream("move to "+dest, err)
	}
	return len(uids), nil
}

// ensureFolder creates folder when it does not exist.
func (s *Service) ensureFolder(account, folder string) error {
	fs, err := s.ListFolders(context.Background(), account)
	if err != nil {
		return err
	}
	for _, f := range fs {
		if strings.EqualFold(f.Path, folder) {
			return nil
		}
	}
	return s.CreateFolder(context.Background(), account, folder)
}

// ParseScreenCommands reads screener commands from an email reply's text
// against a digest's numbering (number = index + 1): "approve 3",
// "approve 3 feed", "approve 3 4 receipts", "block 7", "release all".
// Quoted lines are ignored. Unknown lines are returned as errors.
func ParseScreenCommands(text string, numbering []string) ([]ScreenDecision, []string) {
	var out []ScreenDecision
	var bad []string
	seen := map[string]bool{}
	add := func(d ScreenDecision) {
		if !seen[d.Sender] {
			seen[d.Sender] = true
			out = append(out, d)
		}
	}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r", ""), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, ">") {
			continue
		}
		if strings.HasPrefix(line, "--") || strings.HasPrefix(strings.ToLower(line), "on ") && strings.HasSuffix(line, "wrote:") {
			break // signature or the quoted digest
		}
		f := strings.Fields(strings.ToLower(line))
		switch f[0] {
		case "release":
			if len(f) == 2 && f[1] == "all" {
				for _, sndr := range numbering {
					add(ScreenDecision{Sender: sndr, Action: ScreenApprove, Dest: DestInbox})
				}
				continue
			}
		case ScreenApprove, ScreenBlock:
			dest := ""
			var nums []int
			ok := len(f) > 1
			for _, w := range f[1:] {
				for _, part := range strings.Split(w, ",") {
					if part == "" {
						continue
					}
					if n, err := strconv.Atoi(part); err == nil && n >= 1 && n <= len(numbering) {
						nums = append(nums, n)
					} else if f[0] == ScreenApprove && dest == "" && (part == DestInbox || part == DestFeed || part == DestReceipts) {
						dest = part
					} else {
						ok = false
					}
				}
			}
			if ok && len(nums) > 0 {
				for _, n := range nums {
					add(ScreenDecision{Sender: numbering[n-1], Action: f[0], Dest: dest})
				}
				continue
			}
		}
		if len(bad) < 10 {
			bad = append(bad, line)
		}
	}
	return out, bad
}

// ScreenVia is Screen with the channel recorded (rest, cli).
func (s *Service) ScreenVia(ctx context.Context, p ScreenParams, via string) (ScreenResult, error) {
	p.via = via
	return s.Screen(ctx, p)
}
