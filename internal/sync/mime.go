package sync

import (
	"bytes"
	"io"
	"mime"
	"strings"

	_ "github.com/emersion/go-message/charset" // decode non-UTF-8 charsets
	"github.com/emersion/go-message/mail"

	"github.com/dmz006/imap-mcp/internal/db"
)

// maxPartBytes caps how much of any one text part is kept, so a pathological
// message cannot bloat the cache. Attachments are never stored, only sized.
const maxPartBytes = 2 << 20

// parsedBody is the decoded content of a message.
type parsedBody struct {
	Text        string
	HTML        string
	Attachments []db.Attachment
	References  []string // References header, oldest first
}

// parseBody decodes a raw RFC 822 message: transfer encodings and charsets
// are undone, the first text/plain and text/html parts are kept, and
// attachments are listed by name, type and decoded size. A malformed message
// yields whatever was decoded before the error.
func parseBody(raw []byte) parsedBody {
	var out parsedBody
	r, err := mail.CreateReader(bytes.NewReader(raw))
	if r == nil {
		return out
	}
	if err == nil || r.Header.Header.Len() > 0 {
		if refs, err := r.Header.MsgIDList("References"); err == nil {
			out.References = refs
		}
	}
	for {
		p, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			if p == nil {
				break
			}
		}
		switch h := p.Header.(type) {
		case *mail.InlineHeader:
			ct, _, _ := h.ContentType()
			body, _ := io.ReadAll(io.LimitReader(p.Body, maxPartBytes))
			switch {
			case ct == "text/plain" && out.Text == "":
				out.Text = string(body)
			case ct == "text/html" && out.HTML == "":
				out.HTML = string(body)
			case ct != "text/plain" && ct != "text/html":
				// Inline non-text part (e.g. an image): record like an attachment.
				out.Attachments = append(out.Attachments, db.Attachment{MIME: ct, Size: int64(len(body))})
			}
		case *mail.AttachmentHeader:
			name, _ := h.Filename()
			ct, _, _ := h.ContentType()
			n, _ := io.Copy(io.Discard, p.Body)
			out.Attachments = append(out.Attachments, db.Attachment{Name: decodeWord(name), MIME: ct, Size: n})
		}
	}
	return out
}

var wordDecoder = &mime.WordDecoder{}

func decodeWord(s string) string {
	if d, err := wordDecoder.DecodeHeader(s); err == nil {
		return d
	}
	return s
}

// threadID derives a stable thread key: the root of References, else the
// first In-Reply-To, else the message's own Message-ID.
func threadID(refs, inReplyTo []string, messageID string) string {
	if len(refs) > 0 {
		return strings.Trim(refs[0], "<>")
	}
	if len(inReplyTo) > 0 {
		return strings.Trim(inReplyTo[0], "<>")
	}
	return strings.Trim(messageID, "<>")
}
