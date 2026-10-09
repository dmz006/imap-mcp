package service

import (
	"bytes"
	"context"
	"io"
	"path"
	"strconv"
	"strings"
	"unicode"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-message"
)

// Attachment describes one attachment part (AGENT.md D24).
type Attachment struct {
	Part        string `json:"part"` // IMAP section path, e.g. "2" or "1.3"
	Filename    string `json:"filename,omitempty"`
	MIME        string `json:"mime"`
	Size        int64  `json:"size_bytes"` // encoded size on the server
	Encoding    string `json:"encoding,omitempty"`
	Disposition string `json:"disposition,omitempty"` // attachment | inline | ""
}

// AttachmentContent is one decoded attachment part.
type AttachmentContent struct {
	Attachment
	Data []byte `json:"-"`
}

// ListAttachments lists a message's attachment parts from its live
// BODYSTRUCTURE. It reads no content and never sets \Seen.
func (s *Service) ListAttachments(ctx context.Context, account, folder string, uid uint32) ([]Attachment, error) {
	bs, err := s.bodyStructure(account, folder, uid)
	if err != nil {
		return nil, err
	}
	return attachmentParts(bs), nil
}

// FetchAttachment fetches and decodes one attachment part. maxBytes caps the
// encoded size that will be downloaded (0 = no cap).
func (s *Service) FetchAttachment(ctx context.Context, account, folder string, uid uint32, part string, maxBytes int64) (AttachmentContent, error) {
	path, err := parsePart(part)
	if err != nil {
		return AttachmentContent{}, err
	}
	bs, err := s.bodyStructure(account, folder, uid)
	if err != nil {
		return AttachmentContent{}, err
	}
	var meta *Attachment
	for _, a := range attachmentParts(bs) {
		if a.Part == part {
			a := a
			meta = &a
			break
		}
	}
	if meta == nil {
		return AttachmentContent{}, notFound("part %q is not an attachment of uid=%d in %s; list the attachments first", part, uid, folder)
	}
	if maxBytes > 0 && meta.Size > maxBytes {
		return AttachmentContent{}, &Error{Kind: KindUnprocessable,
			Msg: "attachment is " + strconv.FormatInt(meta.Size, 10) + " bytes, over the tools.attachment_max_mb cap"}
	}

	c, err := s.conn(account)
	if err != nil {
		return AttachmentContent{}, err
	}
	c.Lock()
	defer c.Unlock()
	client := c.Client()
	if _, err := client.Select(folder, &imaplib.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		return AttachmentContent{}, notFound("select %s: %v", folder, err)
	}
	section := &imaplib.FetchItemBodySection{Part: path, Peek: true}
	msgs, err := client.Fetch(imaplib.UIDSetNum(imaplib.UID(uid)), &imaplib.FetchOptions{
		UID: true, BodySection: []*imaplib.FetchItemBodySection{section},
	}).Collect()
	if err != nil {
		return AttachmentContent{}, upstream("fetch part", err)
	}
	if len(msgs) == 0 || len(msgs[0].BodySection) == 0 {
		return AttachmentContent{}, notFound("message uid=%d not found in %s", uid, folder)
	}
	data, err := decodePart(meta.MIME, meta.Encoding, msgs[0].BodySection[0].Bytes)
	if err != nil {
		return AttachmentContent{}, &Error{Kind: KindUnprocessable, Msg: "decode attachment", Err: err}
	}
	return AttachmentContent{Attachment: *meta, Data: data}, nil
}

// bodyStructure fetches a message's BODYSTRUCTURE (read-only select).
func (s *Service) bodyStructure(account, folder string, uid uint32) (imaplib.BodyStructure, error) {
	if folder == "" || uid == 0 {
		return nil, invalid("folder and uid are required")
	}
	c, err := s.conn(account)
	if err != nil {
		return nil, err
	}
	c.Lock()
	defer c.Unlock()
	client := c.Client()
	if _, err := client.Select(folder, &imaplib.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		return nil, notFound("select %s: %v", folder, err)
	}
	msgs, err := client.Fetch(imaplib.UIDSetNum(imaplib.UID(uid)), &imaplib.FetchOptions{
		UID: true, BodyStructure: &imaplib.FetchItemBodyStructure{Extended: true},
	}).Collect()
	if err != nil {
		return nil, upstream("fetch bodystructure", err)
	}
	if len(msgs) == 0 || msgs[0].BodyStructure == nil {
		return nil, notFound("message uid=%d not found in %s", uid, folder)
	}
	return msgs[0].BodyStructure, nil
}

// attachmentParts walks a body structure and returns the parts that are
// attachments: an attachment disposition, a filename, or any non-text leaf
// (an inline image, say). The message's own text/plain and text/html bodies
// are not attachments. A forwarded message (message/rfc822) is one attachment.
func attachmentParts(bs imaplib.BodyStructure) []Attachment {
	out := []Attachment{}
	bs.Walk(func(p []int, part imaplib.BodyStructure) bool {
		single, ok := part.(*imaplib.BodyStructureSinglePart)
		if !ok {
			return true // multipart: descend
		}
		disp := ""
		if d := single.Disposition(); d != nil {
			disp = strings.ToLower(d.Value)
		}
		name := single.Filename()
		mt := single.MediaType()
		isBodyText := (mt == "text/plain" || mt == "text/html") && name == "" && disp != "attachment"
		if isBodyText {
			return false
		}
		out = append(out, Attachment{
			Part: partString(p), Filename: name, MIME: mt,
			Size: int64(single.Size), Encoding: strings.ToLower(single.Encoding), Disposition: disp,
		})
		return false
	})
	return out
}

func partString(p []int) string {
	s := make([]string, len(p))
	for i, n := range p {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ".")
}

// parsePart validates a section path such as "2" or "1.3".
func parsePart(part string) ([]int, error) {
	if part == "" {
		return nil, invalid("part is required")
	}
	fields := strings.Split(part, ".")
	if len(fields) > 16 {
		return nil, invalid("part %q is too deep", part)
	}
	out := make([]int, len(fields))
	for i, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 1 {
			return nil, invalid("part %q must be dot-separated positive numbers, e.g. 2 or 1.3", part)
		}
		out[i] = n
	}
	return out, nil
}

// decodePart undoes the transfer encoding, and for text/* parts the charset
// (go-message decodes both when given the part's headers).
func decodePart(mime, encoding string, raw []byte) ([]byte, error) {
	var h message.Header
	h.Set("Content-Type", mime)
	if encoding != "" {
		h.Set("Content-Transfer-Encoding", encoding)
	}
	e, err := message.New(h, bytes.NewReader(raw))
	if err != nil && e == nil {
		return nil, err
	}
	return io.ReadAll(e.Body)
}

// IsText reports whether a MIME type is text/*.
func (a Attachment) IsText() bool { return strings.HasPrefix(a.MIME, "text/") }

// SafeFilename turns an untrusted attachment or export name into a single
// path component: no directories, no control characters, no leading dots,
// at most 120 bytes. An empty result falls back to def.
func SafeFilename(name, def string) string {
	name = path.Base(strings.ReplaceAll(name, `\`, "/"))
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == '/' || r == 0 || unicode.IsControl(r):
			continue
		case unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune(" ._-+@()[]", r):
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := strings.TrimLeft(strings.TrimSpace(b.String()), ".")
	if len(out) > 120 {
		out = strings.ToValidUTF8(out[len(out)-120:], "")
	}
	if out == "" || out == "." {
		return def
	}
	return out
}
