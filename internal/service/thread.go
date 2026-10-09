package service

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// ThreadParams selects a conversation by its thread_id (AGENT.md D23).
type ThreadParams struct {
	Account  string // "" = every account
	ThreadID string
	Live     bool     // force the live IMAP search even when the cache looks complete
	Folders  []string // live search folders; default: \All if present, else INBOX + \Sent + \Archive
	Limit    int      // max messages (default 100, max 500)
}

// ThreadMessage is one message of a thread.
type ThreadMessage struct {
	Account   string   `json:"account"`
	Folder    string   `json:"folder"`
	UID       uint32   `json:"uid"`
	MessageID string   `json:"message_id,omitempty"`
	Subject   string   `json:"subject"`
	From      string   `json:"from"`
	Date      string   `json:"date"`
	Flags     []string `json:"flags"`
	Source    string   `json:"source"` // cache | live
}

// ThreadResult is a conversation, oldest message first.
type ThreadResult struct {
	ThreadID   string          `json:"thread_id"`
	Count      int             `json:"count"`
	Messages   []ThreadMessage `json:"messages"`
	LiveSearch bool            `json:"live_search"`
	Truncated  bool            `json:"truncated,omitempty"`
	Errors     []AccountError  `json:"errors,omitempty"`
}

// AccountError reports one account's failure in a multi-account operation;
// the other accounts' results are still returned.
type AccountError struct {
	Account string `json:"account"`
	Error   string `json:"error"`
}

// threadLiveFolderCap bounds the UIDs fetched per folder in a live thread search.
const threadLiveFolderCap = 500

// GetThread returns a conversation. It answers from the cache first and falls
// back to a live IMAP search by Message-ID / References / In-Reply-To when no
// message is cached, when the thread's root message is not cached (the thread
// reaches outside the sync window), or when Live is set.
func (s *Service) GetThread(ctx context.Context, p ThreadParams) (ThreadResult, error) {
	p.ThreadID = strings.Trim(strings.TrimSpace(p.ThreadID), "<>")
	if p.ThreadID == "" {
		return ThreadResult{}, invalid("thread_id is required")
	}
	if p.Limit <= 0 || p.Limit > 500 {
		p.Limit = 100
	}
	res := ThreadResult{ThreadID: p.ThreadID, Messages: []ThreadMessage{}}

	cached, rootCached, err := s.cachedThread(ctx, p)
	if err != nil {
		return res, err
	}
	res.Messages = cached

	if p.Live || len(cached) == 0 || !rootCached {
		res.LiveSearch = true
		accounts := s.threadAccounts(p.Account, cached)
		live, errs := s.liveThread(ctx, accounts, p)
		res.Errors = errs
		res.Messages = mergeThread(res.Messages, live)
	}

	sort.SliceStable(res.Messages, func(i, j int) bool { return res.Messages[i].Date < res.Messages[j].Date })
	if len(res.Messages) > p.Limit {
		res.Messages, res.Truncated = res.Messages[:p.Limit], true
	}
	res.Count = len(res.Messages)
	return res, nil
}

// cachedThread reads the thread from the cache and reports whether its root
// message (Message-ID == thread_id) is among the cached rows.
func (s *Service) cachedThread(ctx context.Context, p ThreadParams) ([]ThreadMessage, bool, error) {
	out := []ThreadMessage{}
	if s.db == nil || s.db.SQL() == nil {
		return out, false, nil
	}
	where, args := "thread_id = ?", []any{p.ThreadID}
	if p.Account != "" {
		where, args = where+" AND account = ?", append(args, s.accountName(p.Account))
	}
	rows, err := s.db.SQL().QueryContext(ctx, `SELECT account, folder, uid, COALESCE(message_id,''), COALESCE(subject,''),
		from_addr, COALESCE(from_name,''), COALESCE(internal_date, date), COALESCE(flags,'[]')
		FROM messages WHERE `+where+` ORDER BY COALESCE(internal_date, date) LIMIT 1000`, args...)
	if err != nil {
		return out, false, err
	}
	defer rows.Close()
	root := false
	for rows.Next() {
		var m ThreadMessage
		var name, flags string
		var date int64
		if err := rows.Scan(&m.Account, &m.Folder, &m.UID, &m.MessageID, &m.Subject, &m.From, &name, &date, &flags); err != nil {
			return out, false, err
		}
		m.MessageID = strings.Trim(m.MessageID, "<>")
		if name != "" {
			m.From = name + " <" + m.From + ">"
		}
		m.Date = time.Unix(date, 0).UTC().Format(time.RFC3339)
		m.Flags = []string{}
		_ = json.Unmarshal([]byte(flags), &m.Flags)
		m.Source = "cache"
		if m.MessageID == p.ThreadID {
			root = true
		}
		out = append(out, m)
	}
	return out, root, rows.Err()
}

// threadAccounts picks the accounts to search live: the requested one, else
// the accounts the cached part of the thread is in, else every account.
func (s *Service) threadAccounts(account string, cached []ThreadMessage) []string {
	if account != "" {
		return []string{s.accountName(account)}
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range cached {
		if !seen[m.Account] {
			seen[m.Account] = true
			out = append(out, m.Account)
		}
	}
	if len(out) > 0 {
		return out
	}
	return s.pool.AccountNames()
}

// liveThread searches each account's thread folders on the server.
func (s *Service) liveThread(ctx context.Context, accounts []string, p ThreadParams) ([]ThreadMessage, []AccountError) {
	var out []ThreadMessage
	var errs []AccountError
	for _, acct := range accounts {
		msgs, err := s.liveThreadAccount(ctx, acct, p)
		if err != nil {
			errs = append(errs, AccountError{Account: acct, Error: err.Error()})
			continue
		}
		out = append(out, msgs...)
	}
	return out, errs
}

func (s *Service) liveThreadAccount(ctx context.Context, account string, p ThreadParams) ([]ThreadMessage, error) {
	c, err := s.conn(account)
	if err != nil {
		return nil, err
	}
	c.Lock()
	defer c.Unlock()
	client := c.Client()
	folders := p.Folders
	if len(folders) == 0 {
		if folders, err = threadFolders(client); err != nil {
			return nil, upstream("list folders", err)
		}
	}
	id := p.ThreadID
	criteria := &imaplib.SearchCriteria{Or: [][2]imaplib.SearchCriteria{{
		{Header: []imaplib.SearchCriteriaHeaderField{{Key: "Message-ID", Value: id}}},
		{Or: [][2]imaplib.SearchCriteria{{
			{Header: []imaplib.SearchCriteriaHeaderField{{Key: "References", Value: id}}},
			{Header: []imaplib.SearchCriteriaHeaderField{{Key: "In-Reply-To", Value: id}}},
		}}},
	}}}
	var out []ThreadMessage
	for _, folder := range folders {
		if _, err := client.Select(folder, &imaplib.SelectOptions{ReadOnly: true}).Wait(); err != nil {
			continue // a folder named in the defaults may not exist on this server
		}
		data, err := client.UIDSearch(criteria, nil).Wait()
		if err != nil {
			return out, upstream("search "+folder, err)
		}
		uids := data.AllUIDs()
		if len(uids) == 0 {
			continue
		}
		if len(uids) > threadLiveFolderCap {
			uids = uids[len(uids)-threadLiveFolderCap:]
		}
		msgs, err := client.Fetch(imaplib.UIDSetNum(uids...), headerFetch()).Collect()
		if err != nil {
			return out, upstream("fetch "+folder, err)
		}
		for _, m := range msgs {
			h := msgToHeader(m)
			out = append(out, ThreadMessage{
				Account: account, Folder: folder, UID: h.UID, MessageID: h.MessageID,
				Subject: h.Subject, From: h.From, Date: utcRFC3339(h.Date), Flags: h.Flags, Source: "live",
			})
		}
	}
	return out, nil
}

// threadFolders is the default live search scope: the \All mailbox when the
// server has one (Gmail's All Mail holds every message of a thread), else
// INBOX plus the \Sent and \Archive mailboxes.
func threadFolders(client *imapclient.Client) ([]string, error) {
	boxes, err := client.List("", "*", &imaplib.ListOptions{ReturnSpecialUse: true}).Collect()
	if err != nil {
		return nil, err
	}
	folders := []string{"INBOX"}
	for _, b := range boxes {
		for _, attr := range b.Attrs {
			switch attr {
			case imaplib.MailboxAttrAll:
				return []string{b.Mailbox}, nil
			case imaplib.MailboxAttrSent, imaplib.MailboxAttrArchive:
				folders = append(folders, b.Mailbox)
			}
		}
	}
	return folders, nil
}

// mergeThread adds live messages not already present, matching on Message-ID
// (else account/folder/UID). Cached copies win.
func mergeThread(cached, live []ThreadMessage) []ThreadMessage {
	key := func(m ThreadMessage) string {
		if m.MessageID != "" {
			return m.Account + "\x00" + m.MessageID
		}
		return m.Account + "\x00" + m.Folder + "\x00" + strconv.FormatUint(uint64(m.UID), 10)
	}
	seen := map[string]bool{}
	for _, m := range cached {
		seen[key(m)] = true
	}
	for _, m := range live {
		if k := key(m); !seen[k] {
			seen[k] = true
			cached = append(cached, m)
		}
	}
	return cached
}

// utcRFC3339 normalises an RFC 3339 time to UTC so dates sort as strings.
func utcRFC3339(v string) string {
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return v
	}
	return t.UTC().Format(time.RFC3339)
}
