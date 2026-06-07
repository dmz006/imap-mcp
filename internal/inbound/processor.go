// Package inbound turns trust-gated email into verified command events.
//
// A Processor evaluates a parsed message against an account's gates and, only
// when every required gate passes and the verb is capability-allowed, publishes
// an EventInboundCommand. Anything else publishes EventInboundRejected (for
// audit) and never becomes actionable. This is the imap-mcp trust boundary;
// downstream consumers act only on EventInboundCommand.
package inbound

import (
	"strings"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/config"
	"github.com/dmz006/imap-mcp/internal/trust"
)

// Processor wires the trust verifier to the event bus.
type Processor struct {
	bus      *bus.Bus
	verifier *trust.Verifier
}

func NewProcessor(b *bus.Bus, v *trust.Verifier) *Processor {
	return &Processor{bus: b, verifier: v}
}

// Process evaluates one message for an account and publishes the outcome.
// Returns the full result for callers/tests. Messages with no command envelope
// are ignored silently (not every inbound mail is a command).
func (p *Processor) Process(acct *config.AccountConfig, msg *trust.Message) *trust.Result {
	res := p.verifier.Evaluate(acct, msg)
	switch {
	case res.Triggered:
		if p.bus != nil {
			p.bus.Publish(bus.Event{Type: bus.EventInboundCommand, Account: acct.Name, Payload: res.Verified})
		}
	case hasEnvelope(msg.Body):
		// A command was attempted but failed a gate — audit it.
		if p.bus != nil {
			p.bus.Publish(bus.Event{Type: bus.EventInboundRejected, Account: acct.Name, Payload: res})
		}
	}
	return res
}

func hasEnvelope(body string) bool {
	return strings.Contains(body, "-----BEGIN DATAWATCH COMMAND-----")
}
