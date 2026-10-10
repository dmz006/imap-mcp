package service

import (
	"bytes"
	"context"
	"fmt"
	"regexp"
	"time"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// ExportParams selects what to export (AGENT.md D25). Exactly one selector is
// used: UID (one message → .eml), or for a batch (.mbox) UIDs, ThreadID or From.
type ExportParams struct {
	Account, Folder string
	UID             uint32
	UIDs            []uint32
	ThreadID        string
	From            string
	MaxMessages     int   // batch cap on message count (0 = no cap)
	MaxBytes        int64 // cap on total RFC822 size (0 = no cap)
}

// ExportRef identifies one exported message.
type ExportRef struct {
	Account string `json:"account"`
	Folder  string `json:"folder"`
	UID     uint32 `json:"uid"`
}

// ExportResult is the exported content. Data is never rendered to clients
// directly by the MCP layer; it is written to the working-dir sandbox.
type ExportResult struct {
	Format string `json:"format"` // eml | mbox
	Count  int    `json:"count"`
	// Missing counts thread messages that no longer exist where the thread
	// lookup found them (moved or deleted since); they are skipped.
	Missing  int         `json:"missing,omitempty"`
	Bytes    int         `json:"bytes"`
	Messages []ExportRef `json:"messages"`
	Data     []byte      `json:"-"`
}

// Export fetches raw messages (BODY.PEEK[], so \Seen is never set) and returns
// one .eml or an mboxrd .mbox. Caps are checked against RFC822.SIZE before any
// content is downloaded; over a cap the export is refused, never truncated.
func (s *Service) Export(ctx context.Context, p ExportParams) (ExportResult, error) {
	selectors := 0
	for _, set := range []bool{p.UID != 0, len(p.UIDs) > 0, p.ThreadID != "", p.From != ""} {
		if set {
			selectors++
		}
	}
	if selectors != 1 {
		return ExportResult{}, invalid("give exactly one of uid, uids, thread_id or from")
	}

	var refs []ExportRef
	format := "mbox"
	switch {
	case p.UID != 0:
		if p.Folder == "" {
			return ExportResult{}, invalid("folder is required with uid")
		}
		refs, format = []ExportRef{{Account: s.accountName(p.Account), Folder: p.Folder, UID: p.UID}}, "eml"
	case len(p.UIDs) > 0:
		if p.Folder == "" {
			return ExportResult{}, invalid("folder is required with uids")
		}
		for _, u := range p.UIDs {
			refs = append(refs, ExportRef{Account: s.accountName(p.Account), Folder: p.Folder, UID: u})
		}
	case p.ThreadID != "":
		limit := 500
		if p.MaxMessages > 0 && p.MaxMessages < limit {
			limit = p.MaxMessages + 1 // one over, so an oversize thread is detected
		}
		// Live and preferred: the cache can still list a message at a UID a
		// rule has since moved; the server's location is the one to fetch.
		th, err := s.GetThread(ctx, ThreadParams{Account: p.Account, ThreadID: p.ThreadID, Limit: limit, Live: true, PreferLive: true})
		if err != nil {
			return ExportResult{}, err
		}
		if len(th.Errors) > 0 && len(th.Messages) == 0 {
			return ExportResult{}, upstream("thread lookup", fmt.Errorf("%s: %s", th.Errors[0].Account, th.Errors[0].Error))
		}
		for _, m := range th.Messages {
			refs = append(refs, ExportRef{Account: m.Account, Folder: m.Folder, UID: m.UID})
		}
	case p.From != "":
		folder := p.Folder
		if folder == "" {
			folder = "INBOX"
		}
		res, err := s.Search(ctx, SearchParams{Account: p.Account, Folder: folder, From: p.From, Limit: 500})
		if err != nil {
			return ExportResult{}, err
		}
		if p.MaxMessages > 0 && res.TotalMatches > p.MaxMessages {
			return ExportResult{}, overCap(res.TotalMatches, p.MaxMessages)
		}
		if res.TotalMatches > len(res.Messages) {
			return ExportResult{}, &Error{Kind: KindUnprocessable, Msg: fmt.Sprintf(
				"%d messages match; export by sender handles at most %d at a time, narrow the selection", res.TotalMatches, len(res.Messages))}
		}
		for _, m := range res.Messages {
			refs = append(refs, ExportRef{Account: s.accountName(p.Account), Folder: folder, UID: m.UID})
		}
	}
	if len(refs) == 0 {
		return ExportResult{}, notFound("no messages matched")
	}
	if format == "mbox" && p.MaxMessages > 0 && len(refs) > p.MaxMessages {
		return ExportResult{}, overCap(len(refs), p.MaxMessages)
	}

	tolerant := p.ThreadID != "" // a thread may include messages moved since it was looked up
	raws, found, err := s.fetchRaw(refs, p.MaxBytes, tolerant)
	if err != nil {
		return ExportResult{}, err
	}
	if len(raws) == 0 {
		return ExportResult{}, notFound("none of the thread's messages exist where they were found; try again after the next sync")
	}
	res := ExportResult{Format: format, Messages: found, Count: len(raws), Missing: len(refs) - len(found)}
	if format == "eml" {
		res.Data = raws[0].data
	} else {
		var buf bytes.Buffer
		for _, r := range raws {
			writeMboxrd(&buf, r.from, r.date, r.data)
		}
		res.Data = buf.Bytes()
	}
	res.Bytes = len(res.Data)
	return res, nil
}

func overCap(n, max int) error {
	return &Error{Kind: KindUnprocessable, Msg: fmt.Sprintf(
		"%d messages selected, over the tools.export_max_messages cap of %d; narrow the selection or raise the cap", n, max)}
}

type rawMessage struct {
	from string
	date time.Time
	data []byte
}

// fetchRaw downloads refs, grouped by account and folder, in ref order. The
// total RFC822.SIZE is checked against maxBytes before downloading. With
// tolerant, refs that no longer exist are skipped (and left out of found);
// otherwise any missing ref is NotFound.
func (s *Service) fetchRaw(refs []ExportRef, maxBytes int64, tolerant bool) ([]rawMessage, []ExportRef, error) {
	type key struct{ account, folder string }
	var order []key
	groups := map[key][]imaplib.UID{}
	for _, r := range refs {
		k := key{r.Account, r.Folder}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], imaplib.UID(r.UID))
	}

	// Size check first: no content is downloaded for an oversize export.
	var total int64
	for _, k := range order {
		n, err := s.withFolder(k.account, k.folder, func(client *imapclient.Client) (int64, error) {
			msgs, err := client.Fetch(imaplib.UIDSetNum(groups[k]...), &imaplib.FetchOptions{UID: true, RFC822Size: true}).Collect()
			if err != nil {
				return 0, upstream("fetch sizes", err)
			}
			if len(msgs) != len(groups[k]) && !tolerant {
				return 0, notFound("%d of %d messages not found in %s", len(groups[k])-len(msgs), len(groups[k]), k.folder)
			}
			var sum int64
			present := make([]imaplib.UID, 0, len(msgs))
			for _, m := range msgs {
				sum += m.RFC822Size
				present = append(present, m.UID)
			}
			groups[k] = present
			return sum, nil
		})
		if err != nil {
			return nil, nil, err
		}
		total += n
	}
	if maxBytes > 0 && total > maxBytes {
		return nil, nil, &Error{Kind: KindUnprocessable, Msg: fmt.Sprintf(
			"selection is %d bytes, over the tools.export_max_mb cap; narrow the selection or raise the cap", total)}
	}

	byRef := map[ExportRef]rawMessage{}
	for _, k := range order {
		if len(groups[k]) == 0 {
			continue // every message of this folder has gone (tolerant mode)
		}
		_, err := s.withFolder(k.account, k.folder, func(client *imapclient.Client) (int64, error) {
			msgs, err := client.Fetch(imaplib.UIDSetNum(groups[k]...), &imaplib.FetchOptions{
				UID: true, Envelope: true, InternalDate: true,
				BodySection: []*imaplib.FetchItemBodySection{{Peek: true}}, // BODY.PEEK[]
			}).Collect()
			if err != nil {
				return 0, upstream("fetch messages", err)
			}
			for _, m := range msgs {
				r := rawMessage{date: m.InternalDate, from: "MAILER-DAEMON"}
				if m.Envelope != nil && len(m.Envelope.From) > 0 {
					if a := m.Envelope.From[0].Addr(); a != "" {
						r.from = a
					}
				}
				if len(m.BodySection) > 0 {
					r.data = m.BodySection[0].Bytes
				}
				byRef[ExportRef{Account: k.account, Folder: k.folder, UID: uint32(m.UID)}] = r
			}
			return 0, nil
		})
		if err != nil {
			return nil, nil, err
		}
	}
	out := make([]rawMessage, 0, len(refs))
	var found []ExportRef
	for _, r := range refs {
		if m, ok := byRef[r]; ok {
			out = append(out, m)
			found = append(found, r)
		}
	}
	return out, found, nil
}

// withFolder runs fn with the account's connection locked and folder selected
// read-only.
func (s *Service) withFolder(account, folder string, fn func(*imapclient.Client) (int64, error)) (int64, error) {
	c, err := s.conn(account)
	if err != nil {
		return 0, err
	}
	c.Lock()
	defer c.Unlock()
	client := c.Client()
	if _, err := client.Select(folder, &imaplib.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		return 0, notFound("select %s: %v", folder, err)
	}
	return fn(client)
}

var mboxFromLine = regexp.MustCompile(`^>*From `)

// writeMboxrd appends one message in mboxrd format: a "From " separator line,
// the message with CRLF turned into LF and any line matching ^>*From  quoted
// with one more '>', then a blank line.
func writeMboxrd(buf *bytes.Buffer, from string, date time.Time, raw []byte) {
	if date.IsZero() {
		date = time.Unix(0, 0)
	}
	fmt.Fprintf(buf, "From %s %s\n", from, date.UTC().Format("Mon Jan _2 15:04:05 2006"))
	raw = bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	for _, line := range bytes.SplitAfter(raw, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		if mboxFromLine.Match(line) {
			buf.WriteByte('>')
		}
		buf.Write(line)
	}
	if !bytes.HasSuffix(raw, []byte("\n")) {
		buf.WriteByte('\n')
	}
	buf.WriteByte('\n')
}
