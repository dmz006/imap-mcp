package service

import (
	"strings"
	"testing"
	"time"

	"github.com/dmz006/imap-mcp/internal/config"
)

func TestBuildLink(t *testing.T) {
	gmail := config.AccountConfig{IMAP: config.IMAPConfig{Host: "imap.gmail.com"}, Auth: config.AuthConfig{Username: "Jane <jane@gmail.com>"}}
	other := config.AccountConfig{IMAP: config.IMAPConfig{Host: "mail.example.com"}}
	tb := other
	tb.Link = "thunderbird"
	tmpl := other
	tmpl.Link = "https://webmail.example.com/?_task=mail&_mbox={folder}&_uid={uid}&_action=show"
	cases := []struct {
		name string
		a    config.AccountConfig
		id   string
		uid  uint32
		want string
	}{
		{"gmail default", gmail, "<a+b@mail.example.com>", 0, "https://mail.google.com/mail/?authuser=jane%40gmail.com#search/rfc822msgid%3Aa%2Bb%40mail.example.com"},
		{"other default none", other, "<x@example.com>", 7, ""},
		{"thunderbird", tb, "<x y@example.com>", 0, "mid:x%20y@example.com"},
		{"template", tmpl, "<x@example.com>", 42, "https://webmail.example.com/?_task=mail&_mbox=Archive%2F2026&_uid=42&_action=show"},
		{"template needs uid", tmpl, "<x@example.com>", 0, ""},
		{"no id", gmail, "", 3, ""},
	}
	for _, c := range cases {
		if got := buildLink(c.a, c.id, "Archive/2026", c.uid); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
	none := gmail
	none.Link = "none"
	if got := buildLink(none, "<x@example.com>", "", 0); got != "" {
		t.Errorf("none on gmail: %q", got)
	}
}

func TestValidLink(t *testing.T) {
	for _, ok := range []string{"", "gmail", "Thunderbird", "none", "https://w.example.com/?id={message_id}"} {
		if err := config.ValidLink(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"outlook", "https://w.example.com/", "{message_id}"} {
		if config.ValidLink(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestDigestShowsLinks(t *testing.T) {
	waiting := ReplyList{Count: 1, HistoryComplete: true, Items: []ReplyItem{{Counterpart: "pal@example.org", Subject: "lunch", DaysWaiting: 3,
		Link: "https://mail.google.com/mail/?authuser=jane%40gmail.com#search/rfc822msgid%3Ax%40example.org"}}}
	held := []heldRow{{sender: "x@spam.example", subject: "hi", reasons: "new", folder: "Held", link: "mid:h@spam.example", heldAt: time.Now().Unix()}}
	msg := string(digestMessage("jane@gmail.com", "gmail", held, waiting, digestLearning{}, time.Now()))
	for _, want := range []string{"  open: https://mail.google.com/mail/?authuser=jane%40gmail.com#search/rfc822msgid%3Ax%40example.org\r\n", "  open: mid:h@spam.example\r\n"} {
		if !strings.Contains(msg, want) {
			t.Errorf("digest lacks %q", want)
		}
	}
}
