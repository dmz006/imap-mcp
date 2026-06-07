// Package trust implements the inbound command-channel trust boundary.
//
// imap-mcp is the trust boundary for email-as-control-channel: an inbound
// message may only TRIGGER an action if it passes every gate the account
// requires. Gates are composable and configured per account. With no gates,
// nothing triggers (default-deny). A downstream consumer (e.g. a datawatch
// comm backend) should act ONLY on the VerifiedCommand this package emits,
// never on the raw message.
//
// Implemented gates: allowlist, DKIM/DMARC (via the receiving server's
// Authentication-Results), HMAC over the command envelope, and nonce/replay.
// The PGP gate is declared but fails closed until implemented (backlog).
package trust

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/dmz006/imap-mcp/internal/config"
)

// Message is the parsed inbound mail handed to the verifier. It carries only
// what the gates need — no IMAP coupling, so it is trivially testable.
type Message struct {
	From    string            // envelope/header From address
	Headers map[string]string // lower-cased header name → value (last wins)
	Body    string            // decoded text body
}

// Command is the structured, actionable payload extracted from a message body.
// It is only honored once every required gate passes.
type Command struct {
	Verb  string
	Args  string // opaque JSON/text, interpreted by the consumer
	Nonce string
	TS    time.Time
	HMAC  string // hex HMAC-SHA256 over the canonical envelope (if present)
	raw   string // canonical bytes the HMAC is computed over
}

// VerifiedCommand is emitted only for a fully-trusted, capability-allowed command.
type VerifiedCommand struct {
	Account string
	From    string
	Command Command
	Gates   []string // names of the gates that were satisfied
}

// Result is the outcome of evaluating one message against an account's gates.
type Result struct {
	Triggered bool             // true only if all required gates passed AND verb is allowed
	Verified  *VerifiedCommand // set iff Triggered
	Passed    []string         // gates that passed
	Failed    []string         // gates that failed (with reasons in Reasons)
	Reasons   map[string]string
}

func (r *Result) fail(gate, why string) {
	r.Failed = append(r.Failed, gate)
	if r.Reasons == nil {
		r.Reasons = map[string]string{}
	}
	r.Reasons[gate] = why
}

// NonceStore records seen command nonces for replay protection.
type NonceStore interface {
	// SeenOrRecord returns true if the nonce was already recorded; otherwise it
	// records (nonce, ts) and returns false.
	SeenOrRecord(account, nonce string, ts time.Time) (bool, error)
}

// Verifier evaluates inbound messages against per-account gate config.
type Verifier struct {
	nonces NonceStore
	now    func() time.Time
}

func NewVerifier(nonces NonceStore) *Verifier {
	return &Verifier{nonces: nonces, now: time.Now}
}

// Evaluate runs every gate the account requires. Default-deny: if the inbound
// config is missing/disabled, or any required gate fails, or the command verb
// is not in the account's capability list, Triggered is false.
func (v *Verifier) Evaluate(acct *config.AccountConfig, msg *Message) *Result {
	r := &Result{Reasons: map[string]string{}}
	if acct == nil || acct.Inbound == nil || !acct.Inbound.Enabled {
		r.fail("inbound", "inbound command channel not enabled for this account")
		return r
	}
	g := acct.Inbound.Gates

	// A command envelope must be present for anything to trigger.
	cmd, err := parseCommand(msg.Body)
	if err != nil {
		r.fail("envelope", err.Error())
		return r
	}

	// ── Gate: allowlist ──────────────────────────────────────────────────
	if len(g.Allowlist) > 0 {
		if allowlisted(msg.From, g.Allowlist) {
			r.Passed = append(r.Passed, "allowlist")
		} else {
			r.fail("allowlist", fmt.Sprintf("sender %q not in allowlist", msg.From))
		}
	}

	// ── Gate: DKIM / DMARC (via Authentication-Results) ──────────────────
	if g.RequireDKIM {
		if authResultPass(msg.Headers, "dkim") {
			r.Passed = append(r.Passed, "dkim")
		} else {
			r.fail("dkim", "no dkim=pass in Authentication-Results")
		}
	}
	if g.RequireDMARC {
		if authResultPass(msg.Headers, "dmarc") {
			r.Passed = append(r.Passed, "dmarc")
		} else {
			r.fail("dmarc", "no dmarc=pass in Authentication-Results")
		}
	}

	// ── Gate: HMAC over the command envelope ─────────────────────────────
	if g.HMACSecret != "" {
		if cmd.HMAC == "" {
			r.fail("hmac", "command envelope missing hmac")
		} else if verifyHMAC(g.HMACSecret, cmd.raw, cmd.HMAC) {
			r.Passed = append(r.Passed, "hmac")
		} else {
			r.fail("hmac", "hmac mismatch")
		}
	}

	// ── Gate: PGP (BACKLOG — fails closed) ───────────────────────────────
	if g.RequirePGP {
		r.fail("pgp", "pgp gate not yet implemented (backlog) — fails closed")
	}

	// ── Gate: replay / freshness ─────────────────────────────────────────
	// Always enforced when a command is present, so a captured valid command
	// cannot be resent. Window check applies only if configured.
	if g.ReplayWindowMinutes > 0 {
		age := v.now().Sub(cmd.TS)
		if age < 0 {
			age = -age
		}
		if age > time.Duration(g.ReplayWindowMinutes)*time.Minute {
			r.fail("replay", fmt.Sprintf("command timestamp outside %d-min window", g.ReplayWindowMinutes))
		}
	}
	if cmd.Nonce == "" {
		r.fail("replay", "command envelope missing nonce")
	} else if v.nonces != nil {
		seen, err := v.nonces.SeenOrRecord(acct.Name, cmd.Nonce, cmd.TS)
		if err != nil {
			r.fail("replay", "nonce store error: "+err.Error())
		} else if seen {
			r.fail("replay", "nonce already used (replay)")
		} else {
			r.Passed = append(r.Passed, "replay")
		}
	}

	// Default-deny: any failed gate blocks triggering.
	if len(r.Failed) > 0 {
		return r
	}

	// Capability scoping: the verb must be explicitly allowed.
	if !capabilityAllowed(cmd.Verb, acct.Inbound.Capabilities) {
		r.fail("capability", fmt.Sprintf("verb %q not in account capability list", cmd.Verb))
		return r
	}

	r.Triggered = true
	r.Verified = &VerifiedCommand{
		Account: acct.Name,
		From:    msg.From,
		Command: cmd,
		Gates:   r.Passed,
	}
	return r
}

// ── primitive checks (pure, individually testable) ───────────────────────────

// allowlisted matches a sender against entries that are either full addresses
// (user@host) or bare domains (host, matching any user at that domain).
func allowlisted(from string, allow []string) bool {
	addr := strings.ToLower(strings.TrimSpace(extractAddr(from)))
	if addr == "" {
		return false
	}
	domain := addr
	if at := strings.LastIndex(addr, "@"); at >= 0 {
		domain = addr[at+1:]
	}
	for _, e := range allow {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if strings.Contains(e, "@") {
			if e == addr {
				return true
			}
		} else if e == domain {
			return true
		}
	}
	return false
}

// extractAddr pulls the bare address out of a "Name <a@b>" header value.
func extractAddr(s string) string {
	if lt := strings.LastIndex(s, "<"); lt >= 0 {
		if gt := strings.Index(s[lt:], ">"); gt >= 0 {
			return s[lt+1 : lt+gt]
		}
	}
	return strings.TrimSpace(s)
}

// authResultPass reports whether the Authentication-Results header contains a
// "<method>=pass" verdict (e.g. dkim=pass, dmarc=pass).
func authResultPass(headers map[string]string, method string) bool {
	ar := headers["authentication-results"]
	if ar == "" {
		return false
	}
	return strings.Contains(strings.ToLower(ar), method+"=pass")
}

// verifyHMAC checks a hex HMAC-SHA256 over raw using secret, in constant time.
func verifyHMAC(secret, raw, got string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(raw))
	want := mac.Sum(nil)
	gotb, err := hex.DecodeString(strings.TrimSpace(got))
	if err != nil {
		return false
	}
	return hmac.Equal(want, gotb)
}

func capabilityAllowed(verb string, caps []string) bool {
	for _, c := range caps {
		if strings.EqualFold(strings.TrimSpace(c), verb) {
			return true
		}
		if strings.TrimSpace(c) == "*" {
			return true
		}
	}
	return false
}
