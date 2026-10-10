package service

import (
	"bufio"
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	imaplib "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-message"
	"github.com/emersion/go-message/mail"
	"github.com/emersion/go-message/textproto"

	"github.com/dmz006/imap-mcp/internal/intel"
)

// DefaultNewSenderDays is the new_sender window when a rule sets none: a
// sender is new when nothing of theirs is older than this (AGENT.md D30).
const DefaultNewSenderDays = 30

// holdScore is the header score at which a new sender's message is held
// without asking the model; a score of 1 defers to its enrichment hall.
const holdScore = 2

// freemailDomains are shared mailbox providers. Having written to someone at
// one of these says nothing about anyone else there, so they never make a
// whole domain trusted, and mail addressed only to them is not to the owner.
var freemailDomains = map[string]bool{
	"gmail.com": true, "googlemail.com": true, "yahoo.com": true, "hotmail.com": true,
	"outlook.com": true, "live.com": true, "msn.com": true, "aol.com": true,
	"icloud.com": true, "me.com": true, "mac.com": true, "proton.me": true,
	"protonmail.com": true, "gmx.com": true, "mail.com": true, "yandex.com": true,
	"comcast.net": true, "att.net": true, "verizon.net": true,
}

// throwawayTLDs are low-cost TLDs common in one-shot spam domains. Alone they
// add one point, never enough to hold a message.
var throwawayTLDs = map[string]bool{
	"shop": true, "pro": true, "site": true, "top": true, "xyz": true, "click": true,
	"online": true, "store": true, "buzz": true, "icu": true, "rest": true, "cfd": true,
	"sbs": true, "lol": true, "mom": true, "bond": true, "cyou": true, "space": true,
	"ws": true, "live": true, "life": true,
}

// brands maps names that phishing borrows to the domains that may use them.
// A display name containing one, sent from any other domain, is impersonation.
var brands = []struct {
	re      *regexp.Regexp
	domains []string
}{
	{regexp.MustCompile(`\b(quickbooks|qbo|intuit)\b`), []string{"intuit.com"}},
	{regexp.MustCompile(`\b(social security|ssa\.gov|ssa)\b`), []string{"ssa.gov"}},
	{regexp.MustCompile(`\b(irs|internal reven[ue]+ service)\b`), []string{"irs.gov"}},
	{regexp.MustCompile(`\bfederal reserve\b`), []string{"federalreserve.gov", "frb.org"}},
	{regexp.MustCompile(`\bmedicare\b`), []string{"medicare.gov", "cms.gov"}},
	{regexp.MustCompile(`\bstate ?farm`), []string{"statefarm.com"}},
	{regexp.MustCompile(`\blowe[’']?s\b`), []string{"lowes.com"}},
	{regexp.MustCompile(`\bhome depot\b`), []string{"homedepot.com"}},
	{regexp.MustCompile(`\bamazon\b`), []string{"amazon.com", "amazonses.com"}},
	{regexp.MustCompile(`\bpaypal\b`), []string{"paypal.com"}},
	{regexp.MustCompile(`\b(apple|icloud)\b`), []string{"apple.com", "icloud.com"}},
	{regexp.MustCompile(`\b(microsoft|office ?365)\b`), []string{"microsoft.com", "office.com"}},
	{regexp.MustCompile(`\bnetflix\b`), []string{"netflix.com"}},
	{regexp.MustCompile(`\b(norton|mcafee|geek squad)\b`), []string{"norton.com", "mcafee.com", "bestbuy.com"}},
	{regexp.MustCompile(`\bdocusign\b`), []string{"docusign.net", "docusign.com"}},
	{regexp.MustCompile(`\b(fedex|dhl|usps)\b`), []string{"fedex.com", "dhl.com", "usps.com"}},
	{regexp.MustCompile(`\b(chase|wells fargo|bank of america|coinbase)\b`), []string{"chase.com", "wellsfargo.com", "bankofamerica.com", "coinbase.com"}},
	{regexp.MustCompile(`\b(t-mobile|verizon|at&t)\b`), []string{"t-mobile.com", "verizon.com", "verizonwireless.com", "att.com"}},
	{regexp.MustCompile(`\b(cbs|fox) news\b|\bcbs\b`), []string{"cbs.com", "cbsnews.com", "foxnews.com"}},
	{regexp.MustCompile(`\b(aaa|garmin|mychart|nordvpn|unitedhealthcare)\b`), []string{"aaa.com", "garmin.com", "mychart.com", "nordvpn.com", "uhc.com"}},
}

// newSenderGate holds what the new_sender condition needs for one rule run.
type newSenderGate struct {
	account    string
	state      *sql.DB
	cache      *sql.DB
	cutoff     int64
	own        map[string]bool
	ownDomains map[string]bool
	ownLabels  []string        // own domains without their TLD ("example" for example.com)
	releasedBy map[string]bool // senders released in this run (a dry run writes nothing)
}

// heldInfo is one message the gate decided to hold.
type heldInfo struct {
	ref, sender, subject string
	reasons              []string
	hash                 int64
}

// newSenderGate checks that new_sender can be judged for account: the
// intelligence scan is on and has read the account's whole history.
// Otherwise every sender would look new.
func (s *Service) newSenderGate(account string, days int) (*newSenderGate, error) {
	if !s.cfg.Intel.On() {
		return nil, errors.New("new_sender needs the intelligence scan (intelligence.enabled)")
	}
	var folders, complete int
	err := s.db.StateSQL().QueryRow(`SELECT count(*), count(completed_at) FROM intel_scan WHERE account = ?`, account).Scan(&folders, &complete)
	if err != nil {
		return nil, err
	}
	if folders == 0 || complete < folders {
		return nil, fmt.Errorf("new_sender: the history scan of %s is not complete yet (%d/%d folders); nothing was matched", account, complete, folders)
	}
	if days <= 0 {
		days = DefaultNewSenderDays
	}
	g := &newSenderGate{account: account, state: s.db.StateSQL(), cache: s.db.SQL(),
		cutoff: time.Now().AddDate(0, 0, -days).Unix(), own: map[string]bool{}, ownDomains: map[string]bool{}, releasedBy: map[string]bool{}}
	for _, a := range s.cfg.Accounts {
		addrs := []string{a.Auth.Username}
		if a.SMTP != nil {
			addrs = append(addrs, a.SMTP.From)
		}
		for _, addr := range addrs {
			if addr = bareAddr(addr); strings.Contains(addr, "@") {
				g.own[addr] = true
				if d := domainOf(addr); !freemailDomains[d] {
					g.ownDomains[d] = true
				}
			}
		}
	}
	for d := range g.ownDomains {
		if label := strings.Split(d, ".")[0]; len(label) >= 3 {
			g.ownLabels = append(g.ownLabels, label)
		}
	}
	return g, nil
}

// bareAddr lowercases an address and strips a "Name <a@b>" wrapper.
func bareAddr(addr string) string {
	addr = strings.ToLower(strings.TrimSpace(addr))
	if i := strings.LastIndexByte(addr, '<'); i >= 0 {
		addr = strings.TrimSuffix(addr[i+1:], ">")
	}
	return addr
}

func domainOf(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 {
		return strings.ToLower(addr[i+1:])
	}
	return ""
}

// sameOrg reports whether domain d is base or a subdomain of it.
func sameOrg(d, base string) bool { return d == base || strings.HasSuffix(d, "."+base) }

// isNew reports whether addr is a sender with no history: not one of your own
// addresses, never written to or replied to, not released from a hold before,
// nothing from them before the cutoff, and not at a (non-freemail) domain you
// have written to.
func (g *newSenderGate) isNew(addr string) (bool, error) {
	addr = bareAddr(addr)
	if addr == "" {
		return true, nil
	}
	if g.own[addr] {
		return false, nil
	}
	var sent, replied, trusted int
	var first sql.NullInt64
	err := g.state.QueryRow(`SELECT sent_count, reply_count, trusted, first_seen FROM senders WHERE address = ?`, addr).Scan(&sent, &replied, &trusted, &first)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return false, err
	case sent > 0 || replied > 0 || trusted > 0 || (first.Valid && first.Int64 < g.cutoff):
		return false, nil
	}
	domain := domainOf(addr)
	if domain == "" || freemailDomains[domain] {
		return true, nil
	}
	var known int
	err = g.state.QueryRow(`SELECT EXISTS(SELECT 1 FROM senders WHERE domain = ? AND (sent_count > 0 OR trusted > 0))`, domain).Scan(&known)
	if err != nil {
		return false, err
	}
	return known == 0, nil
}

// holdHeaderFields are the extra headers the signals read (PEEK).
var holdHeaderFields = []string{"References", "List-Unsubscribe", "List-Id", "Precedence"}

// filter keeps the uids whose sender is new and whose message looks like bulk
// mail or a scam (D30), returning why each was kept. A message found here that
// was held before has been moved back by the owner: its sender becomes
// trusted and it is skipped.
func (g *newSenderGate) filter(client *imapclient.Client, uids []imaplib.UID, dryRun bool) ([]imaplib.UID, map[imaplib.UID]heldInfo, error) {
	if len(uids) == 0 {
		return nil, nil, nil
	}
	msgs, err := client.Fetch(imaplib.UIDSetNum(uids...), &imaplib.FetchOptions{UID: true, Envelope: true,
		BodySection: []*imaplib.FetchItemBodySection{{Specifier: imaplib.PartSpecifierHeader, HeaderFields: holdHeaderFields, Peek: true}},
	}).Collect()
	if err != nil {
		return nil, nil, fmt.Errorf("fetch senders: %w", err)
	}
	// First pass: messages the owner moved back release their senders, so
	// the second pass skips every message from them, in any order.
	for _, m := range msgs {
		if m.Envelope == nil {
			continue
		}
		hash := intel.MsgHash(m.Envelope.MessageID)
		if hash == 0 {
			continue
		}
		from := ""
		if len(m.Envelope.From) > 0 {
			from = bareAddr(m.Envelope.From[0].Addr())
		}
		released, err := g.released(hash, from, dryRun)
		if err != nil {
			return nil, nil, err
		}
		if released {
			g.releasedBy[from] = true
		}
	}
	var keep []imaplib.UID
	info := map[imaplib.UID]heldInfo{}
	for _, m := range msgs {
		env := m.Envelope
		if env == nil {
			continue
		}
		from := ""
		if len(env.From) > 0 {
			from = bareAddr(env.From[0].Addr())
		}
		if g.releasedBy[from] {
			continue
		}
		hash := intel.MsgHash(env.MessageID)
		ok, err := g.isNew(from)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			continue
		}
		reasons, hold, err := g.judge(env, headerOf(m), from)
		if err != nil {
			return nil, nil, err
		}
		if !hold {
			continue
		}
		keep = append(keep, m.UID)
		info[m.UID] = heldInfo{ref: strings.Trim(env.MessageID, "<>"), sender: from, subject: env.Subject, reasons: reasons, hash: hash}
	}
	return keep, info, nil
}

// released reports whether the message was held before. Finding it again in
// the folder the rule watches means the owner moved it back: the sender is
// trusted from now on (unless this is a dry run).
func (g *newSenderGate) released(hash int64, from string, dryRun bool) (bool, error) {
	var n int
	err := g.state.QueryRow(`SELECT count(*) FROM held_messages WHERE account = ? AND msg_hash = ? AND released_at IS NULL`, g.account, hash).Scan(&n)
	if err != nil || n == 0 || dryRun {
		return n > 0, err
	}
	now := time.Now().Unix()
	if _, err := g.state.Exec(`UPDATE held_messages SET released_at = ? WHERE account = ? AND msg_hash = ?`, now, g.account, hash); err != nil {
		return true, err
	}
	if from != "" {
		if _, err := g.state.Exec(`INSERT INTO senders(address, domain, first_seen, trusted) VALUES(?,?,?,1)
			ON CONFLICT(address) DO UPDATE SET trusted = 1`, from, domainOf(from), now); err != nil {
			return true, err
		}
	}
	return true, nil
}

// judge scores a new sender's message from its headers. A reply to the
// owner's own mail, or copying someone the owner writes to, always stays.
func (g *newSenderGate) judge(env *imaplib.Envelope, h mail.Header, from string) ([]string, bool, error) {
	// Signs of a real first contact.
	ids := []string{}
	ids = append(ids, env.InReplyTo...)
	if refs, err := h.MsgIDList("References"); err == nil {
		ids = append(ids, refs...)
	}
	for _, id := range ids {
		var out int
		if hash := intel.MsgHash(id); hash != 0 {
			if err := g.state.QueryRow(`SELECT EXISTS(SELECT 1 FROM intel_messages WHERE msg_hash = ? AND outgoing = 1)`, hash).Scan(&out); err != nil {
				return nil, false, err
			}
		}
		if out == 1 {
			return nil, false, nil
		}
	}
	var recipients []string
	for _, a := range append(append([]imaplib.Address{}, env.To...), env.Cc...) {
		if addr := bareAddr(a.Addr()); strings.Contains(addr, "@") && domainOf(addr) != "" {
			recipients = append(recipients, addr)
		}
	}
	for _, r := range recipients {
		if g.own[r] || r == from {
			continue
		}
		var known int
		if err := g.state.QueryRow(`SELECT EXISTS(SELECT 1 FROM senders WHERE address = ? AND sent_count > 0)`, r).Scan(&known); err != nil {
			return nil, false, err
		}
		if known == 1 {
			return nil, false, nil
		}
	}

	// Signs of bulk mail or a scam.
	score := 0
	var reasons []string
	add := func(n int, why string) { score += n; reasons = append(reasons, why) }
	domain := domainOf(from)
	name := ""
	if len(env.From) > 0 {
		name = strings.ToLower(env.From[0].Name)
	}
	if why := g.impersonation(name, domain); why != "" {
		add(2, why)
	}
	if notToOwner(recipients, from, g.own) {
		add(1, "not addressed to you")
	}
	if p := strings.ToLower(h.Get("Precedence")); h.Get("List-Unsubscribe") != "" || h.Get("List-Id") != "" || p == "bulk" || p == "list" || p == "junk" {
		add(1, "bulk mail you never signed up for")
	}
	if throwawayDomain(domain) {
		add(1, "throwaway-looking domain")
	}
	subj := strings.ToLower(env.Subject)
	for addr := range g.own {
		if strings.Contains(subj, addr) {
			add(1, "your address in the subject")
			break
		}
	}
	if len(env.ReplyTo) > 0 {
		if rd := domainOf(bareAddr(env.ReplyTo[0].Addr())); rd != "" && domain != "" && !sameOrg(rd, domain) && !sameOrg(domain, rd) {
			add(1, "replies go to another domain")
		}
	}
	switch {
	case score >= holdScore:
		return reasons, true, nil
	case score == 0:
		return nil, false, nil
	}
	// One signal: let the classify model's hall decide. Not classified yet
	// means the message stays; the next run judges it again.
	hall, err := g.hall(env.MessageID)
	if err != nil {
		return nil, false, err
	}
	switch hall {
	case "", "conversation", "personal":
		return nil, false, nil
	}
	return append(reasons, "classified as "+hall), true, nil
}

// impersonation explains a display name that borrows a brand, an agency, an
// address or the owner's own domain while sent from an unrelated domain.
func (g *newSenderGate) impersonation(name, domain string) string {
	if name == "" || domain == "" {
		return ""
	}
	if i := strings.IndexByte(name, '@'); i >= 0 {
		shown := strings.TrimRight(strings.Fields(name[i+1:] + " ")[0], ">)].,;:")
		if strings.Contains(shown, ".") && !sameOrg(domain, shown) {
			return "display name shows another address"
		}
	}
	for _, label := range g.ownLabels {
		if strings.Contains(name, label) && !g.ownDomains[domain] {
			return "display name uses your own domain"
		}
	}
	for _, b := range brands {
		if !b.re.MatchString(name) {
			continue
		}
		for _, d := range b.domains {
			if sameOrg(domain, d) {
				return ""
			}
		}
		return "display name borrows a brand or agency"
	}
	return ""
}

// notToOwner: no recipients, only the sender itself, or only other people's
// freemail addresses.
func notToOwner(recipients []string, from string, own map[string]bool) bool {
	if len(recipients) == 0 {
		return true
	}
	for _, r := range recipients {
		if own[r] || (r != from && !freemailDomains[domainOf(r)]) {
			return false
		}
	}
	return true
}

// throwawayDomain: a low-cost TLD, or a name with digits mixed into letters
// ("ctka5jyx", "norqenix68").
func throwawayDomain(domain string) bool {
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return false
	}
	if throwawayTLDs[labels[len(labels)-1]] {
		return true
	}
	name := labels[len(labels)-2]
	return strings.ContainsFunc(name, unicode.IsDigit) && strings.ContainsFunc(name, unicode.IsLetter)
}

// hall is the enrichment classification of the cached message, if any.
func (g *newSenderGate) hall(messageID string) (string, error) {
	id := strings.Trim(messageID, "<>")
	if id == "" {
		return "", nil
	}
	var hall sql.NullString
	err := g.cache.QueryRow(`SELECT hall FROM messages WHERE account = ? AND message_id IN (?, ?) AND COALESCE(hall,'') <> '' LIMIT 1`,
		g.account, id, "<"+id+">").Scan(&hall)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	// Only a real hall counts; labels stored before 0.15.0 may be copied
	// placeholders ("project or context", "<newsletter>").
	h := strings.Trim(strings.ToLower(strings.TrimSpace(hall.String)), "<>")
	if !validHalls[h] {
		return "", nil
	}
	return h, nil
}

// validHalls are the classify model's halls (enrichment prompt).
var validHalls = map[string]bool{"transactional": true, "conversation": true, "newsletter": true,
	"notification": true, "alert": true, "personal": true}

// record stores what a rule run held, for release detection and the digest.
func (g *newSenderGate) record(info map[imaplib.UID]heldInfo, folder string) error {
	now := time.Now().Unix()
	for _, h := range info {
		if h.hash == 0 {
			continue
		}
		subject := h.subject
		if len(subject) > 200 {
			subject = strings.ToValidUTF8(subject[:200], "")
		}
		if _, err := g.state.Exec(`INSERT INTO held_messages(account, msg_hash, message_ref, sender, subject, reasons, folder, held_at)
			VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(account, msg_hash) DO UPDATE SET held_at = excluded.held_at, released_at = NULL, digested_at = NULL`,
			g.account, h.hash, h.ref, h.sender, subject, strings.Join(h.reasons, "; "), folder, now); err != nil {
			return err
		}
	}
	return nil
}

// headerOf parses the header fields fetched with holdHeaderFields.
func headerOf(m *imapclient.FetchMessageBuffer) mail.Header {
	for _, bs := range m.BodySection {
		if bs.Section == nil || bs.Section.Specifier != imaplib.PartSpecifierHeader {
			continue
		}
		if h, err := textproto.ReadHeader(bufio.NewReader(bytes.NewReader(bs.Bytes))); err == nil {
			return mail.Header{Header: message.Header{Header: h}}
		}
	}
	return mail.Header{}
}
