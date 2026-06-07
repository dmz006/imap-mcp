package smtp

import (
	"strings"
	"testing"
	"time"

	"github.com/dmz006/imap-mcp/internal/config"
)

func testSender(t *testing.T) *Sender {
	t.Helper()
	s, err := NewSender(&config.SMTPConfig{Host: "mail.dmzs.com", Port: 587, From: "Me <me@dmzs.com>"})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Unix(0, 0).UTC() }
	return s
}

func TestBuild_Headers(t *testing.T) {
	s := testSender(t)
	raw := string(s.build(Message{To: []string{"a@x.com", "b@x.com"}, Subject: "hi", Body: "line1\nline2"}))
	for _, want := range []string{
		"From: Me <me@dmzs.com>\r\n",
		"To: a@x.com, b@x.com\r\n",
		"Subject: hi\r\n",
		"Content-Type: text/plain; charset=UTF-8\r\n",
		"line1\r\nline2",
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("message missing %q\n---\n%s", want, raw)
		}
	}
}

func TestBuild_HeaderInjectionStripped(t *testing.T) {
	s := testSender(t)
	raw := string(s.build(Message{To: []string{"a@x.com"}, Subject: "evil\r\nBcc: victim@x.com", Body: "x"}))
	// The injected text must not become its own header line (no CRLF before it).
	if strings.Contains(raw, "\r\nBcc:") {
		t.Fatalf("CRLF injection in subject created a real header:\n%s", raw)
	}
	// And the Subject must remain a single header line.
	subjLine := ""
	for _, ln := range strings.Split(raw, "\r\n") {
		if strings.HasPrefix(ln, "Subject:") {
			subjLine = ln
		}
	}
	if !strings.Contains(subjLine, "evil") || !strings.Contains(subjLine, "victim@x.com") {
		t.Fatalf("subject not collapsed to one line: %q", subjLine)
	}
}

func TestFromAddr(t *testing.T) {
	cases := map[string]string{
		"Me <me@dmzs.com>": "me@dmzs.com",
		"plain@dmzs.com":   "plain@dmzs.com",
		"  spaced@x.com  ": "spaced@x.com",
	}
	for in, want := range cases {
		if got := fromAddr(in); got != want {
			t.Errorf("fromAddr(%q)=%q want %q", in, got, want)
		}
	}
}

func TestNewSender_Validation(t *testing.T) {
	if _, err := NewSender(nil); err == nil {
		t.Error("nil config should error")
	}
	if _, err := NewSender(&config.SMTPConfig{From: "x@y.com"}); err == nil {
		t.Error("missing host should error")
	}
}
