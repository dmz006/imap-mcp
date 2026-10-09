package enrichment

// Cleaner normalises message text before it is embedded or classified
// (AGENT.md D7-D). The cached body is never modified — only the text handed
// to the models. Iteration 3 plugs in real cleaners (HTML→text, quoted-reply
// and signature stripping, OTP/card redaction); iteration 2 ships the hook.
type Cleaner interface {
	Clean(subject, body string) (cleanSubject, cleanBody string)
}

// NopCleaner passes text through unchanged.
type NopCleaner struct{}

func (NopCleaner) Clean(subject, body string) (string, string) { return subject, body }

// SetCleaner replaces the pipeline's cleaner (nil restores the no-op).
func (p *Pipeline) SetCleaner(c Cleaner) {
	if c == nil {
		c = NopCleaner{}
	}
	p.cleaner = c
}
