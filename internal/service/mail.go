package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// FolderInfo is one mailbox.
type FolderInfo struct {
	Path  string   `json:"path"`
	Delim string   `json:"delimiter"`
	Attrs []string `json:"attributes,omitempty"`
}

// MessageHeader is the summary of one message.
type MessageHeader struct {
	UID      uint32   `json:"uid"`
	SeqNum   uint32   `json:"seq_num"`
	Subject  string   `json:"subject"`
	From     string   `json:"from"`
	To       []string `json:"to"`
	Date     string   `json:"date"`
	Flags    []string `json:"flags"`
	Size     int64    `json:"size_bytes"`
	ThreadID string   `json:"thread_id,omitempty"`
}

// MessageDetail is a message with its text body.
type MessageDetail struct {
	MessageHeader
	BodyText string `json:"body_text,omitempty"`
	BodyHTML string `json:"body_html,omitempty"`
}

// MessagePage is one page of a folder listing.
type MessagePage struct {
	Folder   string          `json:"folder"`
	Total    uint32          `json:"total"`
	Offset   int             `json:"offset"`
	Limit    int             `json:"limit"`
	Count    int             `json:"count"`
	Messages []MessageHeader `json:"messages"`
}

// ListFolders lists an account's mailboxes.
func (s *Service) ListFolders(ctx context.Context, account string) ([]FolderInfo, error) {
	c, err := s.conn(account)
	if err != nil {
		return nil, err
	}
	c.Lock()
	defer c.Unlock()
	mailboxes, err := c.Client().List("", "*", nil).Collect()
	if err != nil {
		return nil, upstream("list folders", err)
	}
	out := make([]FolderInfo, 0, len(mailboxes))
	for _, mb := range mailboxes {
		f := FolderInfo{Path: mb.Mailbox, Delim: string(mb.Delim)}
		for _, a := range mb.Attrs {
			f.Attrs = append(f.Attrs, string(a))
		}
		out = append(out, f)
	}
	return out, nil
}

// CreateFolder creates a mailbox.
func (s *Service) CreateFolder(ctx context.Context, account, path string) error {
	if path == "" {
		return invalid("path (or folder) is required")
	}
	c, err := s.conn(account)
	if err != nil {
		return err
	}
	c.Lock()
	defer c.Unlock()
	if err := c.Client().Create(path, nil).Wait(); err != nil {
		return upstream("create folder", err)
	}
	return nil
}

// DeleteFolder deletes a mailbox.
func (s *Service) DeleteFolder(ctx context.Context, account, path string) error {
	if path == "" {
		return invalid("path (or folder) is required")
	}
	c, err := s.conn(account)
	if err != nil {
		return err
	}
	c.Lock()
	defer c.Unlock()
	if err := c.Client().Delete(path).Wait(); err != nil {
		return upstream("delete folder", err)
	}
	return nil
}

// ListMessagesParams pages through a folder by sequence number.
type ListMessagesParams struct {
	Account, Folder string
	Limit, Offset   int
	Ascending       bool // default newest first
}

// ListMessages returns one page of headers, newest first unless Ascending.
func (s *Service) ListMessages(ctx context.Context, p ListMessagesParams) (MessagePage, error) {
	if p.Folder == "" {
		p.Folder = "INBOX"
	}
	if p.Limit <= 0 || p.Limit > 200 {
		p.Limit = 50
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	page := MessagePage{Folder: p.Folder, Offset: p.Offset, Limit: p.Limit, Messages: []MessageHeader{}}
	c, err := s.conn(p.Account)
	if err != nil {
		return page, err
	}
	c.Lock()
	defer c.Unlock()
	client := c.Client()
	mbox, err := client.Select(p.Folder, nil).Wait()
	if err != nil {
		return page, notFound("select %s: %v", p.Folder, err)
	}
	total := mbox.NumMessages
	page.Total = total
	if total == 0 {
		return page, nil
	}
	// Seq 1 = oldest, seq N = newest.
	var seqSet imaplib.SeqSet
	if p.Ascending {
		start, end := uint32(p.Offset+1), uint32(p.Offset+p.Limit)
		if start > total {
			return page, nil
		}
		seqSet.AddRange(start, min(end, total))
	} else {
		if uint32(p.Offset) >= total {
			return page, nil
		}
		end := total - uint32(p.Offset)
		start := uint32(1)
		if int(end) > p.Limit {
			start = end - uint32(p.Limit) + 1
		}
		seqSet.AddRange(start, end)
	}
	msgs, err := client.Fetch(seqSet, &imaplib.FetchOptions{Envelope: true, Flags: true, UID: true, RFC822Size: true, InternalDate: true}).Collect()
	if err != nil {
		return page, upstream("fetch", err)
	}
	for _, m := range msgs {
		page.Messages = append(page.Messages, msgToHeader(m))
	}
	if !p.Ascending {
		reverse(page.Messages)
	}
	page.Count = len(page.Messages)
	return page, nil
}

// GetMessage fetches one message by UID with its text body.
func (s *Service) GetMessage(ctx context.Context, account, folder string, uid uint32) (MessageDetail, error) {
	if folder == "" || uid == 0 {
		return MessageDetail{}, invalid("folder and uid are required")
	}
	c, err := s.conn(account)
	if err != nil {
		return MessageDetail{}, err
	}
	c.Lock()
	defer c.Unlock()
	client := c.Client()
	if _, err := client.Select(folder, nil).Wait(); err != nil {
		return MessageDetail{}, notFound("select %s: %v", folder, err)
	}
	msgs, err := client.Fetch(imaplib.UIDSetNum(imaplib.UID(uid)), &imaplib.FetchOptions{
		Envelope: true, Flags: true, UID: true, RFC822Size: true, InternalDate: true,
		BodySection: []*imaplib.FetchItemBodySection{{Specifier: imaplib.PartSpecifierText, Peek: true}}, // reading must not set \Seen
	}).Collect()
	if err != nil {
		return MessageDetail{}, upstream("uid fetch", err)
	}
	if len(msgs) == 0 {
		return MessageDetail{}, notFound("message uid=%d not found in %s", uid, folder)
	}
	d := MessageDetail{MessageHeader: msgToHeader(msgs[0])}
	for _, bs := range msgs[0].BodySection {
		if bs.Section != nil && bs.Section.Specifier == imaplib.PartSpecifierText {
			d.BodyText = string(bs.Bytes)
		}
	}
	return d, nil
}

// Headers is a message summary plus its raw header block.
type Headers struct {
	MessageHeader
	RawHeaders string `json:"raw_headers"`
}

// GetHeaders fetches a message's headers only.
func (s *Service) GetHeaders(ctx context.Context, account, folder string, uid uint32) (Headers, error) {
	if folder == "" || uid == 0 {
		return Headers{}, invalid("folder and uid are required")
	}
	c, err := s.conn(account)
	if err != nil {
		return Headers{}, err
	}
	c.Lock()
	defer c.Unlock()
	client := c.Client()
	if _, err := client.Select(folder, nil).Wait(); err != nil {
		return Headers{}, notFound("select %s: %v", folder, err)
	}
	msgs, err := client.Fetch(imaplib.UIDSetNum(imaplib.UID(uid)), &imaplib.FetchOptions{
		Envelope: true, Flags: true, UID: true, RFC822Size: true, InternalDate: true,
		BodySection: []*imaplib.FetchItemBodySection{{Specifier: imaplib.PartSpecifierHeader, Peek: true}},
	}).Collect()
	if err != nil {
		return Headers{}, upstream("fetch", err)
	}
	if len(msgs) == 0 {
		return Headers{}, notFound("message uid=%d not found in %s", uid, folder)
	}
	h := Headers{MessageHeader: msgToHeader(msgs[0])}
	for _, bs := range msgs[0].BodySection {
		if bs.Section != nil && bs.Section.Specifier == imaplib.PartSpecifierHeader {
			h.RawHeaders = string(bs.Bytes)
		}
	}
	return h, nil
}

// SearchParams is an IMAP search within one folder.
type SearchParams struct {
	Account, Folder     string
	From, Subject, Text string
	Since, Before       string // YYYY-MM-DD
	Flags               string // seen | unseen | flagged | answered
	Limit               int
}

// SearchResult carries the exact match count and the newest page.
type SearchResult struct {
	Folder       string          `json:"folder"`
	TotalMatches int             `json:"total_matches"`
	Returned     int             `json:"returned"`
	Messages     []MessageHeader `json:"messages"`
}

// Search runs an IMAP UID SEARCH and returns the newest Limit matches.
func (s *Service) Search(ctx context.Context, p SearchParams) (SearchResult, error) {
	if p.Folder == "" {
		p.Folder = "INBOX"
	}
	if p.Limit <= 0 || p.Limit > 500 {
		p.Limit = 50
	}
	res := SearchResult{Folder: p.Folder, Messages: []MessageHeader{}}
	criteria := &imaplib.SearchCriteria{}
	if p.From != "" {
		criteria.Header = append(criteria.Header, imaplib.SearchCriteriaHeaderField{Key: "From", Value: p.From})
	}
	if p.Subject != "" {
		criteria.Header = append(criteria.Header, imaplib.SearchCriteriaHeaderField{Key: "Subject", Value: p.Subject})
	}
	if p.Text != "" {
		criteria.Body = append(criteria.Body, p.Text)
	}
	for _, d := range []struct {
		v   string
		dst *time.Time
		n   string
	}{{p.Since, &criteria.Since, "since"}, {p.Before, &criteria.Before, "before"}} {
		if d.v == "" {
			continue
		}
		t, err := time.Parse("2006-01-02", d.v)
		if err != nil {
			return res, invalid("%s must be YYYY-MM-DD", d.n)
		}
		*d.dst = t
	}
	switch strings.ToLower(p.Flags) {
	case "":
	case "seen":
		criteria.Flag = append(criteria.Flag, imaplib.FlagSeen)
	case "unseen":
		criteria.NotFlag = append(criteria.NotFlag, imaplib.FlagSeen)
	case "flagged":
		criteria.Flag = append(criteria.Flag, imaplib.FlagFlagged)
	case "answered":
		criteria.Flag = append(criteria.Flag, imaplib.FlagAnswered)
	default:
		return res, invalid("flags must be seen, unseen, flagged or answered")
	}

	c, err := s.conn(p.Account)
	if err != nil {
		return res, err
	}
	c.Lock()
	defer c.Unlock()
	client := c.Client()
	if _, err := client.Select(p.Folder, nil).Wait(); err != nil {
		return res, notFound("select %s: %v", p.Folder, err)
	}
	sd, err := client.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return res, upstream("search", err)
	}
	uids := sd.AllUIDs()
	res.TotalMatches = len(uids) // exact: UID SEARCH returns every match
	if len(uids) == 0 {
		return res, nil
	}
	if len(uids) > p.Limit {
		uids = uids[len(uids)-p.Limit:] // newest = highest UIDs
	}
	msgs, err := client.Fetch(imaplib.UIDSetNum(uids...), &imaplib.FetchOptions{Envelope: true, Flags: true, UID: true, RFC822Size: true, InternalDate: true}).Collect()
	if err != nil {
		return res, upstream("fetch results", err)
	}
	for _, m := range msgs {
		res.Messages = append(res.Messages, msgToHeader(m))
	}
	reverse(res.Messages)
	res.Returned = len(res.Messages)
	return res, nil
}

// MoveMessage moves one message, falling back to COPY + delete of that UID
// only when the server lacks MOVE.
func (s *Service) MoveMessage(ctx context.Context, account, folder string, uid uint32, dest string) error {
	if folder == "" || uid == 0 || dest == "" {
		return invalid("folder, uid, and destination are required")
	}
	c, err := s.conn(account)
	if err != nil {
		return err
	}
	c.Lock()
	defer c.Unlock()
	client := c.Client()
	if _, err := client.Select(folder, nil).Wait(); err != nil {
		return notFound("select %s: %v", folder, err)
	}
	if err := MoveUIDs(client, imaplib.UIDSetNum(imaplib.UID(uid)), dest); err != nil {
		return upstream("move failed", err)
	}
	return nil
}

// CopyMessage copies one message.
func (s *Service) CopyMessage(ctx context.Context, account, folder string, uid uint32, dest string) error {
	if folder == "" || uid == 0 || dest == "" {
		return invalid("folder, uid, and destination are required")
	}
	c, err := s.conn(account)
	if err != nil {
		return err
	}
	c.Lock()
	defer c.Unlock()
	if _, err := c.Client().Select(folder, nil).Wait(); err != nil {
		return notFound("select %s: %v", folder, err)
	}
	if _, err := c.Client().Copy(imaplib.UIDSetNum(imaplib.UID(uid)), dest).Wait(); err != nil {
		return upstream("copy", err)
	}
	return nil
}

// DeleteResult says where a deleted message went.
type DeleteResult struct {
	UID       uint32 `json:"uid"`
	Permanent bool   `json:"permanent"`
	Trash     string `json:"trash,omitempty"` // folder it was moved to
	Note      string `json:"note,omitempty"`
}

// DeleteMessage moves a message to Trash, or with permanent removes exactly
// that UID (never other \Deleted messages in the folder).
func (s *Service) DeleteMessage(ctx context.Context, account, folder string, uid uint32, permanent bool) (DeleteResult, error) {
	res := DeleteResult{UID: uid, Permanent: permanent}
	if folder == "" || uid == 0 {
		return res, invalid("folder and uid are required")
	}
	c, err := s.conn(account)
	if err != nil {
		return res, err
	}
	c.Lock()
	defer c.Unlock()
	client := c.Client()
	if _, err := client.Select(folder, nil).Wait(); err != nil {
		return res, notFound("select %s: %v", folder, err)
	}
	set := imaplib.UIDSetNum(imaplib.UID(uid))
	if permanent {
		if err := DeleteUIDs(client, set); err != nil {
			return res, upstream("permanent delete", err)
		}
		return res, nil
	}
	for _, trash := range []string{"Trash", "[Gmail]/Trash"} {
		if err := MoveUIDs(client, set, trash); err == nil {
			res.Trash = trash
			return res, nil
		}
	}
	if err := client.Store(set, &imaplib.StoreFlags{Op: imaplib.StoreFlagsAdd, Flags: []imaplib.Flag{imaplib.FlagDeleted}}, nil).Close(); err != nil {
		return res, upstream("mark deleted", err)
	}
	res.Note = "no trash folder found; marked \\Deleted (not expunged)"
	return res, nil
}

// SetFlags adds and/or removes flags (comma-separated, backslash optional).
func (s *Service) SetFlags(ctx context.Context, account, folder string, uid uint32, add, remove string) error {
	if folder == "" || uid == 0 {
		return invalid("folder and uid are required")
	}
	if add == "" && remove == "" {
		return invalid("add or remove is required")
	}
	c, err := s.conn(account)
	if err != nil {
		return err
	}
	c.Lock()
	defer c.Unlock()
	client := c.Client()
	if _, err := client.Select(folder, nil).Wait(); err != nil {
		return notFound("select %s: %v", folder, err)
	}
	set := imaplib.UIDSetNum(imaplib.UID(uid))
	if add != "" {
		if err := client.Store(set, &imaplib.StoreFlags{Op: imaplib.StoreFlagsAdd, Flags: ParseFlags(add)}, nil).Close(); err != nil {
			return upstream("add flags", err)
		}
	}
	if remove != "" {
		if err := client.Store(set, &imaplib.StoreFlags{Op: imaplib.StoreFlagsDel, Flags: ParseFlags(remove)}, nil).Close(); err != nil {
			return upstream("remove flags", err)
		}
	}
	return nil
}

// MoveUIDs moves exactly the given UIDs: native MOVE when the server has it,
// otherwise COPY + DeleteUIDs. (go-imap's own fallback issues a folder-wide
// EXPUNGE on servers without UIDPLUS, which could destroy other \Deleted mail.)
func MoveUIDs(client *imapclient.Client, set imaplib.UIDSet, dest string) error {
	caps := client.Caps()
	if caps.Has(imaplib.CapMove) || caps.Has(imaplib.CapIMAP4rev2) {
		_, err := client.Move(set, dest).Wait()
		return err
	}
	if _, err := client.Copy(set, dest).Wait(); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	if err := DeleteUIDs(client, set); err != nil {
		return fmt.Errorf("remove originals after copy: %w", err)
	}
	return nil
}

// DeleteUIDs permanently removes exactly the given UIDs from the selected
// folder: \Deleted + UID EXPUNGE. Servers without UIDPLUS fall back to a
// plain EXPUNGE only if no other message in the folder is already \Deleted;
// otherwise it refuses rather than destroy mail the user didn't target.
func DeleteUIDs(client *imapclient.Client, set imaplib.UIDSet) error {
	if err := client.Store(set, &imaplib.StoreFlags{Op: imaplib.StoreFlagsAdd, Flags: []imaplib.Flag{imaplib.FlagDeleted}}, nil).Close(); err != nil {
		return fmt.Errorf("mark deleted: %w", err)
	}
	caps := client.Caps()
	if caps.Has(imaplib.CapUIDPlus) || caps.Has(imaplib.CapIMAP4rev2) {
		return client.UIDExpunge(set).Close()
	}
	sd, err := client.UIDSearch(&imaplib.SearchCriteria{Flag: []imaplib.Flag{imaplib.FlagDeleted}}, nil).Wait()
	if err != nil {
		return fmt.Errorf("check other deleted messages: %w", err)
	}
	for _, u := range sd.AllUIDs() {
		if !set.Contains(u) {
			return fmt.Errorf("server lacks UIDPLUS and other messages are already \\Deleted; refusing a folder-wide EXPUNGE (messages remain marked \\Deleted)")
		}
	}
	return client.Expunge().Close()
}

// ParseFlags turns "seen, \\Flagged" into IMAP flags (backslash optional).
func ParseFlags(s string) []imaplib.Flag {
	var flags []imaplib.Flag
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if !strings.HasPrefix(f, `\`) {
			f = `\` + f
		}
		flags = append(flags, imaplib.Flag(f))
	}
	return flags
}

func msgToHeader(m *imapclient.FetchMessageBuffer) MessageHeader {
	hdr := MessageHeader{UID: uint32(m.UID), SeqNum: m.SeqNum, Size: m.RFC822Size, Flags: []string{}}
	for _, f := range m.Flags {
		hdr.Flags = append(hdr.Flags, string(f))
	}
	if !m.InternalDate.IsZero() {
		hdr.Date = m.InternalDate.Format(time.RFC3339)
	}
	if env := m.Envelope; env != nil {
		hdr.Subject = env.Subject
		if len(env.From) > 0 {
			a := env.From[0]
			if a.Name != "" {
				hdr.From = fmt.Sprintf("%s <%s@%s>", a.Name, a.Mailbox, a.Host)
			} else {
				hdr.From = fmt.Sprintf("%s@%s", a.Mailbox, a.Host)
			}
		}
		for _, a := range env.To {
			hdr.To = append(hdr.To, fmt.Sprintf("%s@%s", a.Mailbox, a.Host))
		}
		if len(env.InReplyTo) > 0 {
			hdr.ThreadID = strings.Join(env.InReplyTo, " ")
		}
	}
	return hdr
}

func reverse[T any](s []T) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}
