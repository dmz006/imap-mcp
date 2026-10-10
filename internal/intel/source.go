package intel

import (
	"context"
	"fmt"
	"slices"
	"strings"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/dmz006/imap-mcp/internal/imap"
)

// Folder is a selectable mailbox and its attributes (SPECIAL-USE included).
type Folder struct {
	Name  string
	Attrs []string
}

// Batch is one fetch of headers from a folder, in ascending UID order.
type Batch struct {
	UIDValidity uint32
	Headers     []Header
	More        bool // UIDs above the last header remain
}

// Source reads mailbox headers. The scanner only ever reads: folders are
// opened read-only and header fields are fetched with PEEK.
type Source interface {
	Accounts() []string
	Folders(ctx context.Context, account string) ([]Folder, error)
	// Fetch returns up to limit messages with UID > afterUID.
	Fetch(ctx context.Context, account, folder string, afterUID uint32, limit int) (Batch, error)
}

// NewIMAPSource reads from the accounts in an IMAP pool.
func NewIMAPSource(pool *imap.Pool) Source { return &imapSource{pool: pool} }

type imapSource struct{ pool *imap.Pool }

func (s *imapSource) Accounts() []string { return s.pool.AccountNames() }

func (s *imapSource) Folders(ctx context.Context, account string) ([]Folder, error) {
	c, err := s.pool.Resolve(account)
	if err != nil {
		return nil, err
	}
	c.Lock()
	defer c.Unlock()
	opts := &imaplib.ListOptions{}
	if c.Client().Caps().Has(imaplib.CapSpecialUse) {
		opts.ReturnSpecialUse = true
	}
	boxes, err := c.Client().List("", "*", opts).Collect()
	if err != nil {
		return nil, err
	}
	out := make([]Folder, 0, len(boxes))
	for _, b := range boxes {
		f := Folder{Name: b.Mailbox}
		for _, a := range b.Attrs {
			f.Attrs = append(f.Attrs, string(a))
		}
		out = append(out, f)
	}
	return out, nil
}

func (s *imapSource) Fetch(ctx context.Context, account, folder string, afterUID uint32, limit int) (Batch, error) {
	var b Batch
	c, err := s.pool.Resolve(account)
	if err != nil {
		return b, err
	}
	c.Lock()
	defer c.Unlock()
	client := c.Client()
	mbox, err := client.Select(folder, &imaplib.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return b, fmt.Errorf("examine %s: %w", folder, err)
	}
	b.UIDValidity = mbox.UIDValidity
	if mbox.NumMessages == 0 {
		return b, nil
	}
	var set imaplib.UIDSet
	set.AddRange(imaplib.UID(afterUID+1), 0) // afterUID+1:*
	data, err := client.UIDSearch(&imaplib.SearchCriteria{UID: []imaplib.UIDSet{set}}, nil).Wait()
	if err != nil {
		return b, fmt.Errorf("search %s: %w", folder, err)
	}
	var uids []imaplib.UID
	for _, u := range data.AllUIDs() {
		if uint32(u) > afterUID { // "n:*" always includes the highest UID, even if below n
			uids = append(uids, u)
		}
	}
	if len(uids) == 0 {
		return b, nil
	}
	slices.Sort(uids)
	if len(uids) > limit {
		uids, b.More = uids[:limit], true
	}
	msgs, err := client.Fetch(imaplib.UIDSetNum(uids...), &imaplib.FetchOptions{
		UID: true, Envelope: true, InternalDate: true, Flags: true,
		BodySection: []*imaplib.FetchItemBodySection{{
			Specifier: imaplib.PartSpecifierHeader, HeaderFields: scanFields, Peek: true,
		}},
	}).Collect()
	if err != nil {
		return b, fmt.Errorf("fetch %s: %w", folder, err)
	}
	for _, m := range msgs {
		h := Header{UID: uint32(m.UID), Date: m.InternalDate, Answered: slices.Contains(m.Flags, imaplib.FlagAnswered)}
		if env := m.Envelope; env != nil {
			h.MessageID = env.MessageID
			h.Subject = env.Subject
			if len(env.InReplyTo) > 0 {
				h.InReplyTo = env.InReplyTo[0]
			}
			if len(env.From) > 0 {
				h.From = addr(env.From[0])
			}
			if len(env.ReplyTo) > 0 {
				h.ReplyTo = addr(env.ReplyTo[0])
			}
			for _, a := range env.To {
				h.To = append(h.To, addr(a))
			}
			for _, a := range env.Cc {
				h.Cc = append(h.Cc, addr(a))
			}
			if h.Date.IsZero() {
				h.Date = env.Date
			}
		}
		for _, bs := range m.BodySection {
			parseFields(&h, bs.Bytes)
		}
		b.Headers = append(b.Headers, h)
	}
	slices.SortFunc(b.Headers, func(x, y Header) int { return int(x.UID) - int(y.UID) })
	return b, nil
}

func addr(a imaplib.Address) Address {
	return Address{Name: decodeName(a.Name), Addr: strings.ToLower(a.Addr())}
}
