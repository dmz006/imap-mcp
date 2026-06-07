package inbound

import (
	"strings"

	"github.com/dmz006/imap-mcp/internal/trust"
)

// buildMessage assembles a trust.Message from the parts the watcher fetches:
// the envelope From address, a raw header block (for Authentication-Results and
// similar), and the decoded text body.
func buildMessage(from, rawHeaders, body string) *trust.Message {
	return &trust.Message{
		From:    from,
		Headers: parseHeaderBlock(rawHeaders),
		Body:    body,
	}
}

// parseHeaderBlock parses an RFC 5322 header block into a lower-cased map.
// Continuation lines (leading whitespace) are folded into the previous header.
// Later occurrences of a header overwrite earlier ones.
func parseHeaderBlock(raw string) map[string]string {
	out := map[string]string{}
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	var curKey, curVal string
	flush := func() {
		if curKey != "" {
			out[curKey] = strings.TrimSpace(curVal)
		}
	}
	for _, line := range strings.Split(raw, "\n") {
		if line == "" {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			// continuation of the previous header value
			curVal += " " + strings.TrimSpace(line)
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		flush()
		curKey = strings.ToLower(strings.TrimSpace(k))
		curVal = strings.TrimSpace(v)
	}
	flush()
	return out
}
