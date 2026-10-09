package sync

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	imaplib "github.com/emersion/go-imap/v2"

	"github.com/dmz006/imap-mcp/internal/imap"
)

// ErrUIDValidityChanged aborts a folder cycle when the server's UIDVALIDITY
// differs from the one the cycle started with; the next cycle rebuilds.
var ErrUIDValidityChanged = errors.New("uidvalidity changed during sync")

// Folder is a mailbox with its SPECIAL-USE attributes (e.g. \Sent).
type Folder struct {
	Name  string
	Attrs []string
}

// FolderStatus is what EXAMINE reports for a folder.
type FolderStatus struct {
	UIDValidity   uint32
	HighestModSeq uint64 // 0 when CONDSTORE is unavailable
	CondStore     bool
}

// Fetched is one message as fetched from the server.
type Fetched struct {
	UID          uint32
	Flags        []string
	InternalDate time.Time
	Size         int64
	Envelope     *imaplib.Envelope
	Raw          []byte // full RFC 822 message; nil when over the size cap
}

// FlagUpdate is the current flag set of one message.
type FlagUpdate struct {
	UID   uint32
	Flags []string
}

// Source is the read-only view of one account's mailbox that the syncer
// needs. It is an interface so the sync logic can be tested without a server
// and alternative sources (e.g. JMAP) can be plugged in later.
//
// Every folder operation takes the UIDVALIDITY the cycle started with and
// fails with ErrUIDValidityChanged if the server's differs.
type Source interface {
	Folders(ctx context.Context) ([]Folder, error)
	Status(ctx context.Context, folder string) (FolderStatus, error)
	// SearchSince returns UIDs with INTERNALDATE on/after since, plus every
	// \\Flagged message when orFlagged is set.
	SearchSince(ctx context.Context, folder string, validity uint32, since time.Time, orFlagged bool) ([]uint32, error)
	Fetch(ctx context.Context, folder string, validity uint32, uids []uint32, maxBytes int64) ([]Fetched, error)
	Flags(ctx context.Context, folder string, validity uint32, uids []uint32, changedSince uint64) ([]FlagUpdate, error)
}

// imapSource implements Source over a shared pool connection. Each call takes
// the connection lock, EXAMINEs the folder (read-only, so nothing — not even
// \Seen — changes on the server) and releases the lock, so interactive MCP
// tools are never blocked for a whole backfill.
type imapSource struct {
	conn *imap.Conn
}

// NewIMAPSource wraps a pool connection as a Source.
func NewIMAPSource(conn *imap.Conn) Source { return &imapSource{conn: conn} }

func (s *imapSource) Folders(ctx context.Context) ([]Folder, error) {
	s.conn.Lock()
	defer s.conn.Unlock()
	c := s.conn.Client()
	opts := &imaplib.ListOptions{}
	if c.Caps().Has(imaplib.CapSpecialUse) {
		opts.ReturnSpecialUse = true
	}
	list, err := c.List("", "*", opts).Collect()
	if err != nil {
		return nil, err
	}
	out := make([]Folder, 0, len(list))
	for _, l := range list {
		f := Folder{Name: l.Mailbox}
		for _, a := range l.Attrs {
			f.Attrs = append(f.Attrs, string(a))
		}
		out = append(out, f)
	}
	return out, nil
}

// examine selects folder read-only. Caller holds the lock.
func (s *imapSource) examine(folder string) (FolderStatus, error) {
	c := s.conn.Client()
	cond := c.Caps().Has(imaplib.CapCondStore)
	sd, err := c.Select(folder, &imaplib.SelectOptions{ReadOnly: true, CondStore: cond}).Wait()
	if err != nil {
		return FolderStatus{}, fmt.Errorf("examine %s: %w", folder, err)
	}
	return FolderStatus{UIDValidity: sd.UIDValidity, HighestModSeq: sd.HighestModSeq, CondStore: cond && sd.HighestModSeq > 0}, nil
}

func (s *imapSource) examineChecked(folder string, validity uint32) error {
	st, err := s.examine(folder)
	if err != nil {
		return err
	}
	if st.UIDValidity != validity {
		return ErrUIDValidityChanged
	}
	return nil
}

func (s *imapSource) Status(ctx context.Context, folder string) (FolderStatus, error) {
	s.conn.Lock()
	defer s.conn.Unlock()
	return s.examine(folder)
}

func (s *imapSource) SearchSince(ctx context.Context, folder string, validity uint32, since time.Time, orFlagged bool) ([]uint32, error) {
	s.conn.Lock()
	defer s.conn.Unlock()
	if err := s.examineChecked(folder, validity); err != nil {
		return nil, err
	}
	criteria := &imaplib.SearchCriteria{Since: since}
	if orFlagged {
		criteria = &imaplib.SearchCriteria{Or: [][2]imaplib.SearchCriteria{{
			{Since: since}, {Flag: []imaplib.Flag{imaplib.FlagFlagged}},
		}}}
	}
	sd, err := s.conn.Client().UIDSearch(criteria, nil).Wait()
	if err != nil {
		return nil, err
	}
	uids := sd.AllUIDs()
	out := make([]uint32, len(uids))
	for i, u := range uids {
		out[i] = uint32(u)
	}
	return out, nil
}

func uidSet(uids []uint32) imaplib.UIDSet {
	var set imaplib.UIDSet
	for _, u := range uids {
		set.AddNum(imaplib.UID(u))
	}
	return set
}

func flagStrings(fs []imaplib.Flag) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, string(f))
	}
	return out
}

func (s *imapSource) Fetch(ctx context.Context, folder string, validity uint32, uids []uint32, maxBytes int64) ([]Fetched, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	s.conn.Lock()
	defer s.conn.Unlock()
	if err := s.examineChecked(folder, validity); err != nil {
		return nil, err
	}
	c := s.conn.Client()
	// Pass 1: metadata for every UID.
	meta, err := c.Fetch(uidSet(uids), &imaplib.FetchOptions{
		UID: true, Flags: true, InternalDate: true, RFC822Size: true, Envelope: true,
	}).Collect()
	if err != nil {
		return nil, err
	}
	out := make([]Fetched, 0, len(meta))
	var small []uint32
	idx := map[uint32]int{}
	for _, m := range meta {
		f := Fetched{UID: uint32(m.UID), Flags: flagStrings(m.Flags), InternalDate: m.InternalDate, Size: m.RFC822Size, Envelope: m.Envelope}
		idx[f.UID] = len(out)
		out = append(out, f)
		if maxBytes <= 0 || m.RFC822Size <= maxBytes {
			small = append(small, f.UID)
		}
	}
	// Pass 2: full bodies (BODY.PEEK[]) for messages under the size cap.
	if len(small) > 0 {
		section := &imaplib.FetchItemBodySection{Peek: true}
		bodies, err := c.Fetch(uidSet(small), &imaplib.FetchOptions{UID: true, BodySection: []*imaplib.FetchItemBodySection{section}}).Collect()
		if err != nil {
			return nil, err
		}
		for _, b := range bodies {
			if i, ok := idx[uint32(b.UID)]; ok {
				out[i].Raw = b.FindBodySection(section)
			}
		}
	}
	return out, nil
}

func (s *imapSource) Flags(ctx context.Context, folder string, validity uint32, uids []uint32, changedSince uint64) ([]FlagUpdate, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	s.conn.Lock()
	defer s.conn.Unlock()
	if err := s.examineChecked(folder, validity); err != nil {
		return nil, err
	}
	opts := &imaplib.FetchOptions{UID: true, Flags: true}
	if changedSince > 0 {
		opts.ChangedSince = changedSince
	}
	msgs, err := s.conn.Client().Fetch(uidSet(uids), opts).Collect()
	if err != nil {
		return nil, err
	}
	out := make([]FlagUpdate, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, FlagUpdate{UID: uint32(m.UID), Flags: flagStrings(m.Flags)})
	}
	return out, nil
}

// resolveFolder maps a config entry to a mailbox name: SPECIAL-USE tokens
// (leading backslash) match folder attributes case-insensitively; anything
// else is a literal name. ok is false when nothing matches.
func resolveFolder(entry string, folders []Folder) (string, bool) {
	if !strings.HasPrefix(entry, `\`) {
		for _, f := range folders {
			if f.Name == entry || (strings.EqualFold(entry, "INBOX") && strings.EqualFold(f.Name, "INBOX")) {
				return f.Name, true
			}
		}
		return "", false
	}
	for _, f := range folders {
		for _, a := range f.Attrs {
			if strings.EqualFold(a, entry) {
				return f.Name, true
			}
		}
	}
	return "", false
}
