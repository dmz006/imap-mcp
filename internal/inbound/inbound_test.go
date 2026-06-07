package inbound

import (
	"fmt"
	"testing"
	"time"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/trust"
)

type memNonces struct{ seen map[string]bool }

func (m *memNonces) SeenOrRecord(a, n string, _ time.Time) (bool, error) {
	k := a + "|" + n
	if m.seen[k] {
		return true, nil
	}
	m.seen[k] = true
	return false, nil
}

func TestParseHeaderBlock_FoldingAndLowercase(t *testing.T) {
	raw := "From: ops@dmzs.com\r\nAuthentication-Results: mx;\r\n dkim=pass;\r\n dmarc=pass\r\n"
	h := parseHeaderBlock(raw)
	if h["from"] != "ops@dmzs.com" {
		t.Errorf("from=%q", h["from"])
	}
	ar := h["authentication-results"]
	if ar != "mx; dkim=pass; dmarc=pass" {
		t.Errorf("folded auth-results wrong: %q", ar)
	}
}

func envelope(verb, nonce string, ts time.Time) string {
	return fmt.Sprintf("hi\n\n-----BEGIN DATAWATCH COMMAND-----\nverb: %s\nnonce: %s\nts: %s\n-----END DATAWATCH COMMAND-----\n",
		verb, nonce, ts.UTC().Format(time.RFC3339))
}

func newProc() (*Processor, *bus.Bus) {
	b := bus.New()
	return NewProcessor(b, trust.NewVerifier(&memNonces{seen: map[string]bool{}})), b
}

func TestProcess_EmitsVerifiedCommand(t *testing.T) {
	p, b := newProc()
	got := make(chan bus.Event, 1)
	b.Subscribe(bus.EventInboundCommand, func(e bus.Event) { got <- e })

	acct := &config.AccountConfig{
		Name: "a",
		Inbound: &config.InboundConfig{
			Enabled:      true,
			Gates:        config.GateConfig{Allowlist: []string{"dmzs.com"}},
			Capabilities: []string{"status"},
		},
	}
	res := p.Process(acct, &trust.Message{From: "ops@dmzs.com", Body: envelope("status", "n1", time.Now())})
	if !res.Triggered {
		t.Fatalf("expected trigger: %v", res.Reasons)
	}
	select {
	case e := <-got:
		if e.Type != bus.EventInboundCommand || e.Account != "a" {
			t.Fatalf("bad event: %+v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("no EventInboundCommand published")
	}
}

func TestProcess_RejectedEnvelopeAudited(t *testing.T) {
	p, b := newProc()
	got := make(chan bus.Event, 1)
	b.Subscribe(bus.EventInboundRejected, func(e bus.Event) { got <- e })

	acct := &config.AccountConfig{
		Name: "a",
		Inbound: &config.InboundConfig{
			Enabled:      true,
			Gates:        config.GateConfig{Allowlist: []string{"dmzs.com"}},
			Capabilities: []string{"status"},
		},
	}
	// sender not allowlisted → rejected, but it carried an envelope → audited
	res := p.Process(acct, &trust.Message{From: "evil@elsewhere.com", Body: envelope("status", "n2", time.Now())})
	if res.Triggered {
		t.Fatal("must not trigger for non-allowlisted sender")
	}
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("expected EventInboundRejected for failed command attempt")
	}
}

func TestProcess_NoEnvelopeIsSilent(t *testing.T) {
	p, b := newProc()
	fired := make(chan bus.Event, 2)
	b.Subscribe(bus.EventInboundCommand, func(e bus.Event) { fired <- e })
	b.Subscribe(bus.EventInboundRejected, func(e bus.Event) { fired <- e })

	acct := &config.AccountConfig{
		Name:    "a",
		Inbound: &config.InboundConfig{Enabled: true, Gates: config.GateConfig{Allowlist: []string{"dmzs.com"}}, Capabilities: []string{"status"}},
	}
	p.Process(acct, &trust.Message{From: "ops@dmzs.com", Body: "just a normal email, no command here"})
	select {
	case e := <-fired:
		t.Fatalf("ordinary mail must not emit events, got %v", e.Type)
	case <-time.After(200 * time.Millisecond):
		// good — silent
	}
}
