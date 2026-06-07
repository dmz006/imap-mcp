package trust

import (
	"fmt"
	"strings"
	"time"
)

const (
	envBegin = "-----BEGIN DATAWATCH COMMAND-----"
	envEnd   = "-----END DATAWATCH COMMAND-----"
)

// parseCommand extracts a command envelope from a message body. The envelope is
// a fenced block of `key: value` lines:
//
//	-----BEGIN DATAWATCH COMMAND-----
//	verb: mail.archive
//	args: {"folder":"INBOX","from":"noise@example.com"}
//	nonce: 7f3c1a...
//	ts: 2026-06-07T20:00:00Z
//	hmac: <hex hmac-sha256 over the canonical envelope>
//	-----END DATAWATCH COMMAND-----
//
// The HMAC (when used) is computed over the canonical form of verb/args/nonce/ts
// — the hmac line itself is excluded — so signer and verifier agree byte-for-byte.
func parseCommand(body string) (Command, error) {
	start := strings.Index(body, envBegin)
	if start < 0 {
		return Command{}, fmt.Errorf("no command envelope found")
	}
	rest := body[start+len(envBegin):]
	end := strings.Index(rest, envEnd)
	if end < 0 {
		return Command{}, fmt.Errorf("command envelope not terminated")
	}
	block := rest[:end]

	var c Command
	fields := map[string]string{}
	for _, line := range strings.Split(block, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		k, val, ok := strings.Cut(line, ":")
		if !ok {
			return Command{}, fmt.Errorf("malformed envelope line: %q", line)
		}
		fields[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(val)
	}

	c.Verb = fields["verb"]
	c.Args = fields["args"]
	c.Nonce = fields["nonce"]
	c.HMAC = fields["hmac"]
	if c.Verb == "" {
		return Command{}, fmt.Errorf("envelope missing verb")
	}
	if tsStr := fields["ts"]; tsStr != "" {
		ts, err := time.Parse(time.RFC3339, tsStr)
		if err != nil {
			return Command{}, fmt.Errorf("envelope ts not RFC3339: %w", err)
		}
		c.TS = ts
	}
	c.raw = canonicalEnvelope(c)
	return c, nil
}

// canonicalEnvelope is the exact byte sequence the HMAC is computed over.
// Keep this in lock-step with any signer implementation.
func canonicalEnvelope(c Command) string {
	var b strings.Builder
	b.WriteString("verb:" + c.Verb + "\n")
	b.WriteString("args:" + c.Args + "\n")
	b.WriteString("nonce:" + c.Nonce + "\n")
	b.WriteString("ts:" + c.TS.UTC().Format(time.RFC3339) + "\n")
	return b.String()
}

// CanonicalEnvelope exposes the canonical signing form for tooling/tests that
// need to produce a matching HMAC.
func CanonicalEnvelope(verb, args, nonce string, ts time.Time) string {
	return canonicalEnvelope(Command{Verb: verb, Args: args, Nonce: nonce, TS: ts})
}
