package intel

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"mime"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-message/textproto"
)

// Address is one mailbox from an envelope.
type Address struct {
	Name string
	Addr string // lower-case
}

// Header is what the scanner reads from one message: envelope fields plus a
// few header fields. It never holds a subject or a body.
type Header struct {
	UID       uint32
	Date      time.Time // INTERNALDATE
	MessageID string
	InReplyTo string
	From      Address
	To, Cc    []Address

	List   bool   // List-Id or List-Unsubscribe present
	Bulk   bool   // Precedence: bulk, list or junk
	Auto   bool   // Auto-Submitted present and not "no"
	DKIM   string // pass | fail | "" (no result); from the receiving server's Authentication-Results
	DMARC  string // pass | fail | ""
	NoMsID bool   // Message-ID was missing
}

// scanFields are the header fields fetched with BODY.PEEK[HEADER.FIELDS (...)].
var scanFields = []string{"List-Id", "List-Unsubscribe", "Precedence", "Auto-Submitted", "Authentication-Results"}

var (
	dkimRe  = regexp.MustCompile(`(?i)\bdkim\s*=\s*([a-z]+)`)
	dmarcRe = regexp.MustCompile(`(?i)\bdmarc\s*=\s*([a-z]+)`)
)

// parseFields fills the list, bulk, auto and authentication flags from the
// raw HEADER.FIELDS bytes. Only the first Authentication-Results header is
// used: the topmost one is added by the receiving server, later ones could
// have been forged by the sender.
func parseFields(h *Header, raw []byte) {
	hdr, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(raw)))
	if err != nil && hdr.Len() == 0 {
		return
	}
	h.List = hdr.Get("List-Id") != "" || hdr.Get("List-Unsubscribe") != ""
	switch strings.ToLower(strings.TrimSpace(hdr.Get("Precedence"))) {
	case "bulk", "list", "junk":
		h.Bulk = true
	}
	if v := strings.ToLower(strings.TrimSpace(hdr.Get("Auto-Submitted"))); v != "" && v != "no" {
		h.Auto = true
	}
	if ar := hdr.Get("Authentication-Results"); ar != "" { // Get returns the first (topmost)
		h.DKIM = authResult(dkimRe, ar)
		h.DMARC = authResult(dmarcRe, ar)
	}
}

// authResult maps an Authentication-Results method result to pass or fail;
// every result other than pass (fail, none, neutral, temperror, ...) is a
// non-pass. No result for the method gives "".
func authResult(re *regexp.Regexp, ar string) string {
	m := re.FindStringSubmatch(ar)
	if m == nil {
		return ""
	}
	if strings.EqualFold(m[1], "pass") {
		return "pass"
	}
	return "fail"
}

// msgHash is the D28 index key: the first 8 bytes of SHA-256 of the
// normalised Message-ID, as a signed integer for SQLite.
func msgHash(id string) int64 {
	id = strings.ToLower(strings.Trim(strings.TrimSpace(id), "<>"))
	if id == "" {
		return 0
	}
	sum := sha256.Sum256([]byte(id))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

// keyHash is msgHash for a message without a Message-ID: it identifies the
// copy (folder, UID, date, sender), so each copy is counted once.
func keyHash(folder string, h Header) int64 {
	return msgHash("noid:" + folder + "|" + strconv.FormatUint(uint64(h.UID), 10) + "|" +
		h.Date.UTC().Format(time.RFC3339) + "|" + h.From.Addr)
}

var wordDecoder = &mime.WordDecoder{}

// decodeName undoes RFC 2047 encoding in a display name.
func decodeName(s string) string {
	if d, err := wordDecoder.DecodeHeader(s); err == nil {
		return strings.TrimSpace(d)
	}
	return strings.TrimSpace(s)
}

// domainOf returns the lower-case domain of an address ("" if none).
func domainOf(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 && i < len(addr)-1 {
		return strings.ToLower(addr[i+1:])
	}
	return ""
}
