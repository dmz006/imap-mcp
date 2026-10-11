package service

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"mime"
	"net/textproto"
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/config"
	imapsync "github.com/dmz006/imap-mcp/internal/sync"
	"github.com/dmz006/imap-mcp/internal/trust"
)

// screenDigest is a stored digest's numbering and token.
type screenDigest struct {
	id        int64
	ref       string
	token     string
	numbering []string
	used      bool
}

// latestScreenDigest returns the account's newest digest with held senders.
func (s *Service) latestScreenDigest(ctx context.Context, account string) (*screenDigest, error) {
	var d screenDigest
	var nums string
	var used *int64
	err := s.db.StateSQL().QueryRowContext(ctx, `SELECT id, message_ref, token, numbering, used_at FROM screen_digests
		WHERE account = ? ORDER BY id DESC LIMIT 1`, account).Scan(&d.id, &d.ref, &d.token, &nums, &used)
	if err != nil {
		return nil, err
	}
	d.used = used != nil
	if err := json.Unmarshal([]byte(nums), &d.numbering); err != nil {
		return nil, err
	}
	return &d, nil
}

// RunScreenReplies polls for digest replies while email commands are in
// token mode (D42). It returns when ctx ends.
func (s *Service) RunScreenReplies(ctx context.Context) {
	if s.cfg.Rules.Screener.EmailMode() != config.ScreenEmailToken {
		return
	}
	t := time.NewTicker(s.cfg.Rules.Screener.Poll())
	defer t.Stop()
	for {
		if err := s.ScreenReplyTick(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("screener: reply check", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// screenReply is a candidate reply read from the inbox.
type screenReply struct {
	uid       imaplib.UID
	from      string
	inReplyTo string
	authRes   []string
	text      string
}

// ScreenReplyTick handles digest replies in each account's INBOX (token
// mode, D42). A reply counts only when it answers the account's latest
// digest, comes from the account's own address, passes DKIM and DMARC in
// the receiving server's Authentication-Results, and carries the digest's
// unused token. Anything else is logged and ignored. Handled replies are
// marked seen and answered.
func (s *Service) ScreenReplyTick(ctx context.Context) error {
	for _, a := range s.cfg.Accounts {
		owner := bareAddr(a.Auth.Username)
		if owner == "" {
			continue
		}
		d, err := s.latestScreenDigest(ctx, a.Name)
		if err != nil {
			continue // no digest with held senders yet
		}
		replies, err := s.screenReplies(a.Name, owner)
		if err != nil {
			return err
		}
		for _, r := range replies {
			if !strings.Contains(r.inReplyTo, "@imap-mcp.invalid") {
				continue
			}
			var done int
			_ = s.db.StateSQL().QueryRowContext(ctx, `SELECT count(*) FROM screen_replies WHERE account = ? AND uid = ?`, a.Name, int64(r.uid)).Scan(&done)
			if done > 0 {
				continue
			}
			status := s.screenReplyStatus(r, d, owner)
			if status == "ok" {
				decisions, bad := ParseScreenCommands(r.text, d.numbering)
				res, err := s.Screen(ctx, ScreenParams{Account: a.Name, Decisions: decisions, Confirm: true, via: "email-token"})
				if err != nil {
					status = "error"
					slog.Warn("screener: reply", "account", a.Name, "err", err)
				} else {
					_, _ = s.db.StateSQL().ExecContext(ctx, `UPDATE screen_digests SET used_at = ? WHERE id = ?`, time.Now().Unix(), d.id)
					s.screenConfirm(a.Name, owner, res, bad)
				}
			} else {
				slog.Warn("screener: digest reply rejected", "account", a.Name, "reason", status)
			}
			_, _ = s.db.StateSQL().ExecContext(ctx, `INSERT INTO screen_replies(account, uid, status, created_at) VALUES(?,?,?,?)`,
				a.Name, int64(r.uid), status, time.Now().Unix())
			s.markReplyHandled(a.Name, r.uid)
		}
	}
	return nil
}

// screenReplyStatus checks a reply against the gates; "ok" or the reason.
func (s *Service) screenReplyStatus(r screenReply, d *screenDigest, owner string) string {
	switch {
	case !strings.Contains(r.inReplyTo, strings.Trim(d.ref, "<>")):
		return "not a reply to the latest digest"
	case d.used:
		return "token already used"
	case !strings.EqualFold(r.from, owner):
		return "not from the account's own address"
	case !authPass(r.authRes, "dkim") || !authPass(r.authRes, "dmarc"):
		return "no dkim=pass and dmarc=pass in Authentication-Results"
	case !strings.Contains(r.text, d.token):
		return "token missing"
	}
	return "ok"
}

// authPass reports whether any Authentication-Results header says
// method=pass.
func authPass(results []string, method string) bool {
	for _, v := range results {
		if strings.Contains(strings.ToLower(v), method+"=pass") {
			return true
		}
	}
	return false
}

// screenReplies reads recent INBOX mail from the owner's own address.
func (s *Service) screenReplies(account, owner string) ([]screenReply, error) {
	var out []screenReply
	_, err := s.withFolder(account, "INBOX", func(client *imapclient.Client) (int64, error) {
		sd, err := client.UIDSearch(&imaplib.SearchCriteria{
			Header: []imaplib.SearchCriteriaHeaderField{{Key: "From", Value: owner}},
			Since:  time.Now().AddDate(0, 0, -3),
		}, nil).Wait()
		if err != nil {
			return 0, upstream("search", err)
		}
		uids := sd.AllUIDs()
		if len(uids) == 0 {
			return 0, nil
		}
		if len(uids) > 50 {
			uids = uids[len(uids)-50:]
		}
		msgs, err := client.Fetch(imaplib.UIDSetNum(uids...), &imaplib.FetchOptions{
			UID: true, Envelope: true, BodySection: []*imaplib.FetchItemBodySection{{Peek: true}},
		}).Collect()
		if err != nil {
			return 0, upstream("fetch", err)
		}
		for _, m := range msgs {
			if m.Envelope == nil || len(m.Envelope.From) == 0 || len(m.BodySection) == 0 {
				continue
			}
			raw := m.BodySection[0].Bytes
			r := screenReply{uid: m.UID, from: strings.ToLower(m.Envelope.From[0].Addr()), inReplyTo: strings.Join(m.Envelope.InReplyTo, " ")}
			if hdr, err := readHeaderBlock(raw); err == nil {
				r.authRes = hdr["Authentication-Results"]
				if refs := strings.Join(hdr["References"], " "); r.inReplyTo == "" {
					r.inReplyTo = refs
				}
			}
			r.text, _ = imapsync.PlainText(raw)
			out = append(out, r)
		}
		return 0, nil
	})
	return out, err
}

// markReplyHandled flags a handled reply seen and answered.
func (s *Service) markReplyHandled(account string, uid imaplib.UID) {
	c, err := s.conn(account)
	if err != nil {
		return
	}
	c.Lock()
	defer c.Unlock()
	client := c.Client()
	if _, err := client.Select("INBOX", nil).Wait(); err != nil {
		return
	}
	_ = client.Store(imaplib.UIDSetNum(uid), &imaplib.StoreFlags{Op: imaplib.StoreFlagsAdd,
		Flags: []imaplib.Flag{imaplib.FlagSeen, imaplib.FlagAnswered}}, nil).Close()
}

// HandleInboundScreen runs a verified `screen` command from the inbound
// command channel (hmac mode, D42). Args: {"account", "commands"} with the
// digest's numbers, or {"account", "decisions": [...]}.
func (s *Service) HandleInboundScreen(e bus.Event) {
	if s.cfg.Rules.Screener.EmailMode() != config.ScreenEmailHMAC {
		return
	}
	vc, ok := e.Payload.(*trust.VerifiedCommand)
	if !ok || !strings.EqualFold(vc.Command.Verb, "screen") {
		return
	}
	var args struct {
		Account   string           `json:"account"`
		Commands  string           `json:"commands"`
		Decisions []ScreenDecision `json:"decisions"`
	}
	if err := json.Unmarshal([]byte(vc.Command.Args), &args); err != nil {
		slog.Warn("screener: inbound command args", "err", err)
		return
	}
	if args.Account == "" {
		args.Account = e.Account
	}
	ctx := context.Background()
	account := s.accountName(args.Account)
	decisions := args.Decisions
	var bad []string
	if args.Commands != "" {
		d, err := s.latestScreenDigest(ctx, account)
		if err != nil {
			slog.Warn("screener: inbound command: no digest to number from", "account", account)
			return
		}
		decisions, bad = ParseScreenCommands(strings.ReplaceAll(args.Commands, ";", "\n"), d.numbering)
	}
	res, err := s.Screen(ctx, ScreenParams{Account: account, Decisions: decisions, Confirm: true, via: "email-hmac"})
	if err != nil {
		slog.Warn("screener: inbound command", "account", account, "err", err)
		return
	}
	owner := ""
	for _, a := range s.cfg.Accounts {
		if a.Name == account {
			owner = bareAddr(a.Auth.Username)
		}
	}
	s.screenConfirm(account, owner, res, bad)
}

// screenConfirm APPENDs a short confirmation to the account's INBOX.
func (s *Service) screenConfirm(account, owner string, res ScreenResult, bad []string) {
	var b strings.Builder
	approved, blocked := 0, 0
	for _, o := range res.Outcomes {
		switch {
		case o.Error != "":
			fmt.Fprintf(&b, "- %s: not done (%s)\r\n", o.Sender, o.Error)
		case o.Action == ScreenApprove:
			approved++
			fmt.Fprintf(&b, "- approved %s: %d message(s) to %s", o.Sender, o.Moved, o.Folder)
			if o.Rule != nil {
				fmt.Fprintf(&b, "; rule %d moves future mail there (%s)", o.Rule.ID, map[bool]string{true: "active", false: "inactive"}[o.Rule.Active])
			}
			b.WriteString("\r\n")
		default:
			blocked++
			fmt.Fprintf(&b, "- blocked %s: %d message(s) to %s; rule %d trashes mail from %s (%s, matches %d in your inbox now)\r\n",
				o.Sender, o.Moved, o.Folder, o.Rule.ID, o.Rule.From, map[bool]string{true: "active", false: "inactive"}[o.Rule.Active], o.Rule.Matches)
		}
	}
	for _, l := range bad {
		fmt.Fprintf(&b, "- not understood: %q\r\n", l)
	}
	if b.Len() == 0 {
		b.WriteString("No commands found in your reply.\r\n")
	}
	b.WriteString("-- \r\nimap-mcp screener\r\n")
	now := time.Now()
	subject := fmt.Sprintf("Screener: %d approved, %d blocked", approved, blocked)
	var msg strings.Builder
	if owner != "" {
		fmt.Fprintf(&msg, "From: imap-mcp <%s>\r\nTo: <%s>\r\n", owner, owner)
	} else {
		msg.WriteString("From: imap-mcp\r\n")
	}
	fmt.Fprintf(&msg, "Subject: %s\r\nDate: %s\r\nMessage-ID: <screener-%s-%d@imap-mcp.invalid>\r\n", mime.QEncoding.Encode("utf-8", subject),
		now.Format(time.RFC1123Z), account, now.UnixNano())
	msg.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
	msg.WriteString(b.String())
	raw := []byte(msg.String())
	c, err := s.conn(account)
	if err != nil {
		return
	}
	c.Lock()
	defer c.Unlock()
	cmd := c.Client().Append("INBOX", int64(len(raw)), &imaplib.AppendOptions{Time: now})
	_, _ = cmd.Write(raw)
	_ = cmd.Close()
	if _, err := cmd.Wait(); err != nil {
		slog.Warn("screener: append confirmation", "account", account, "err", err)
	}
}

// readHeaderBlock parses a raw message's header (canonical keys).
func readHeaderBlock(raw []byte) (textproto.MIMEHeader, error) {
	h, err := textproto.NewReader(bufio.NewReader(bytes.NewReader(raw))).ReadMIMEHeader()
	if err != nil && len(h) == 0 {
		return nil, err
	}
	return h, nil
}
