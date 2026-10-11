package service

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/dmz006/imap-mcp/internal/config"
)

// messageLink returns a link that opens a message in the owner's mail app
// (D59), or "" when the account has none or the message can't be named.
func (s *Service) messageLink(account, messageID, folder string, uid uint32) string {
	for _, a := range s.cfg.Accounts {
		if a.Name == account {
			return buildLink(a, messageID, folder, uid)
		}
	}
	return ""
}

func buildLink(a config.AccountConfig, messageID, folder string, uid uint32) string {
	id := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(messageID), "<"), ">")
	switch style := a.LinkStyle(); style {
	case config.LinkNone:
		return ""
	case config.LinkGmail:
		if id == "" {
			return ""
		}
		// Gmail opens any message by search; authuser picks the right
		// account when several are signed in.
		return "https://mail.google.com/mail/?authuser=" + url.QueryEscape(bareAddr(a.Auth.Username)) +
			"#search/rfc822msgid%3A" + url.QueryEscape(id)
	case config.LinkThunderbird:
		if id == "" {
			return ""
		}
		return "mid:" + url.PathEscape(id) // RFC 2392; Thunderbird's own "Copy Message Link" form
	default:
		if (strings.Contains(style, "{message_id}") && id == "") || (strings.Contains(style, "{uid}") && uid == 0) {
			return ""
		}
		return strings.NewReplacer("{message_id}", url.QueryEscape(id), "{folder}", url.QueryEscape(folder),
			"{uid}", strconv.FormatUint(uint64(uid), 10)).Replace(style)
	}
}
