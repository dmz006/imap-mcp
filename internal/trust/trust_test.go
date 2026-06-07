package trust

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dmz006/imap-mcp/internal/config"
)

// memNonces is an in-memory NonceStore for tests.
type memNonces struct{ seen map[string]bool }

func newMemNonces() *memNonces { return &memNonces{seen: map[string]bool{}} }

func (m *memNonces) SeenOrRecord(account, nonce string, _ time.Time) (bool, error) {
	k := account + "|" + nonce
	if m.seen[k] {
		return true, nil
	}
	m.seen[k] = true
	return false, nil
}

func hmacHex(secret, raw string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(raw))
	return hex.EncodeToString(mac.Sum(nil))
}

// envelope builds a command-envelope body. Pass hmacSecret="" for no hmac line.
func envelope(verb, args, nonce string, ts time.Time, hmacSecret string) string {
	var h string
	if hmacSecret != "" {
		h = "hmac: " + hmacHex(hmacSecret, CanonicalEnvelope(verb, args, nonce, ts)) + "\n"
	}
	return fmt.Sprintf("Please run this.\n\n%s\nverb: %s\nargs: %s\nnonce: %s\nts: %s\n%s%s\n",
		envBegin, verb, args, nonce, ts.UTC().Format(time.RFC3339), h, envEnd)
}

func acct(gates config.GateConfig, caps ...string) *config.AccountConfig {
	return &config.AccountConfig{
		Name: "acct",
		Inbound: &config.InboundConfig{
			Enabled:      true,
			Gates:        gates,
			Capabilities: caps,
		},
	}
}

func TestEvaluate_DefaultDeny_InboundDisabled(t *testing.T) {
	v := NewVerifier(newMemNonces())
	a := &config.AccountConfig{Name: "x"} // no Inbound
	r := v.Evaluate(a, &Message{From: "ops@dmzs.com", Body: envelope("status", "", "n1", time.Now(), "")})
	if r.Triggered {
		t.Fatal("must not trigger when inbound is not enabled")
	}
}

func TestEvaluate_Allowlist(t *testing.T) {
	v := NewVerifier(newMemNonces())
	ts := time.Now()
	body := envelope("status", "", "n-allow", ts, "")

	// domain-form allowlist, allowed sender
	a := acct(config.GateConfig{Allowlist: []string{"dmzs.com"}}, "status")
	if r := v.Evaluate(a, &Message{From: "Ops <ops@dmzs.com>", Body: body}); !r.Triggered {
		t.Fatalf("expected trigger, failed: %v", r.Reasons)
	}
	// not allowlisted
	a2 := acct(config.GateConfig{Allowlist: []string{"dmzs.com"}}, "status")
	if r := v.Evaluate(a2, &Message{From: "evil@elsewhere.com", Body: envelope("status", "", "n-allow2", ts, "")}); r.Triggered {
		t.Fatal("must not trigger for non-allowlisted sender")
	}
}

func TestEvaluate_DKIM_DMARC(t *testing.T) {
	v := NewVerifier(newMemNonces())
	ts := time.Now()
	a := acct(config.GateConfig{RequireDKIM: true, RequireDMARC: true}, "status")

	pass := &Message{
		From:    "ops@dmzs.com",
		Headers: map[string]string{"authentication-results": "mx.dmzs.com; dkim=pass header.d=dmzs.com; dmarc=pass"},
		Body:    envelope("status", "", "n-dkim", ts, ""),
	}
	if r := v.Evaluate(a, pass); !r.Triggered {
		t.Fatalf("expected trigger with dkim+dmarc pass, failed: %v", r.Reasons)
	}

	fail := &Message{
		From:    "ops@dmzs.com",
		Headers: map[string]string{"authentication-results": "mx; dkim=fail; dmarc=fail"},
		Body:    envelope("status", "", "n-dkim2", ts, ""),
	}
	if r := v.Evaluate(acct(config.GateConfig{RequireDKIM: true}, "status"), fail); r.Triggered {
		t.Fatal("must not trigger when dkim=fail")
	}
}

func TestEvaluate_HMAC(t *testing.T) {
	v := NewVerifier(newMemNonces())
	ts := time.Now()
	secret := "shared-secret"
	a := acct(config.GateConfig{HMACSecret: secret}, "mail.archive")

	good := &Message{From: "ops@dmzs.com", Body: envelope("mail.archive", `{"folder":"INBOX"}`, "n-hmac", ts, secret)}
	if r := v.Evaluate(a, good); !r.Triggered {
		t.Fatalf("expected trigger with valid hmac, failed: %v", r.Reasons)
	}

	// tampered args invalidate the hmac
	bad := &Message{From: "ops@dmzs.com", Body: envelope("mail.archive", `{"folder":"INBOX"}`, "n-hmac2", ts, "wrong-secret")}
	if r := v.Evaluate(acct(config.GateConfig{HMACSecret: secret}, "mail.archive"), bad); r.Triggered {
		t.Fatal("must not trigger with wrong hmac")
	}
}

func TestEvaluate_Replay(t *testing.T) {
	store := newMemNonces()
	v := NewVerifier(store)
	ts := time.Now()
	a := acct(config.GateConfig{Allowlist: []string{"dmzs.com"}, ReplayWindowMinutes: 10}, "status")

	msg := &Message{From: "ops@dmzs.com", Body: envelope("status", "", "same-nonce", ts, "")}
	if r := v.Evaluate(a, msg); !r.Triggered {
		t.Fatalf("first use should trigger: %v", r.Reasons)
	}
	// same nonce again → replay
	msg2 := &Message{From: "ops@dmzs.com", Body: envelope("status", "", "same-nonce", ts, "")}
	if r := v.Evaluate(a, msg2); r.Triggered {
		t.Fatal("replayed nonce must not trigger")
	}

	// stale timestamp outside window
	old := time.Now().Add(-30 * time.Minute)
	stale := &Message{From: "ops@dmzs.com", Body: envelope("status", "", "fresh-nonce", old, "")}
	if r := v.Evaluate(a, stale); r.Triggered {
		t.Fatal("stale command outside window must not trigger")
	}
}

func TestEvaluate_PGP_FailsClosed(t *testing.T) {
	v := NewVerifier(newMemNonces())
	a := acct(config.GateConfig{RequirePGP: true}, "status")
	r := v.Evaluate(a, &Message{From: "ops@dmzs.com", Body: envelope("status", "", "n-pgp", time.Now(), "")})
	if r.Triggered {
		t.Fatal("pgp gate must fail closed until implemented")
	}
	if r.Reasons["pgp"] == "" {
		t.Fatal("expected a pgp failure reason")
	}
}

func TestEvaluate_CapabilityScoping(t *testing.T) {
	v := NewVerifier(newMemNonces())
	ts := time.Now()
	// gates pass, but verb not in capability list
	a := acct(config.GateConfig{Allowlist: []string{"dmzs.com"}}, "status")
	r := v.Evaluate(a, &Message{From: "ops@dmzs.com", Body: envelope("mail.delete", "", "n-cap", ts, "")})
	if r.Triggered {
		t.Fatal("verb outside capability list must not trigger")
	}
	if !strings.Contains(strings.Join(r.Failed, ","), "capability") {
		t.Fatalf("expected capability failure, got %v", r.Failed)
	}
}

func TestEvaluate_FullStack_HappyPath(t *testing.T) {
	v := NewVerifier(newMemNonces())
	ts := time.Now()
	secret := "s3cr3t"
	a := acct(config.GateConfig{
		Allowlist:           []string{"ops@dmzs.com"},
		RequireDKIM:         true,
		RequireDMARC:        true,
		HMACSecret:          secret,
		ReplayWindowMinutes: 5,
	}, "mail.archive")

	msg := &Message{
		From:    "Ops <ops@dmzs.com>",
		Headers: map[string]string{"authentication-results": "mx; dkim=pass; dmarc=pass"},
		Body:    envelope("mail.archive", `{"from":"noise@x.com"}`, "n-full", ts, secret),
	}
	r := v.Evaluate(a, msg)
	if !r.Triggered {
		t.Fatalf("full happy path should trigger, failed: %v", r.Reasons)
	}
	if r.Verified == nil || r.Verified.Command.Verb != "mail.archive" {
		t.Fatalf("verified command not populated correctly: %+v", r.Verified)
	}
	// all five gates should be recorded as passed
	for _, want := range []string{"allowlist", "dkim", "dmarc", "hmac", "replay"} {
		found := false
		for _, p := range r.Passed {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("expected gate %q in passed set %v", want, r.Passed)
		}
	}
}
