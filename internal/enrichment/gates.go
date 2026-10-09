package enrichment

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	gosync "sync"
	"time"
)

// LoadGate decides whether backfill enrichment may run right now (AGENT.md
// D11b). Gates only ever pause the backfill lane; new mail is never gated.
// A gate that cannot reach its data source allows backfill (degrades to the
// caps and rate limit) and says why.
type LoadGate interface {
	Name() string
	Allow(ctx context.Context, now time.Time) (ok bool, reason string)
}

// windowGate allows backfill only inside a local-time window that may wrap
// midnight ("quiet hours"). start/end are minutes after midnight.
type windowGate struct{ start, end int }

// NewWindowGate returns a gate for [start, end) minutes after midnight.
func NewWindowGate(start, end int) LoadGate { return windowGate{start, end} }

func (w windowGate) Name() string { return "backfill_window" }

func (w windowGate) Allow(_ context.Context, now time.Time) (bool, string) {
	m := now.Hour()*60 + now.Minute()
	in := m >= w.start && m < w.end
	if w.start > w.end { // wraps midnight, e.g. 22:00-06:00
		in = m >= w.start || m < w.end
	}
	if in {
		return true, ""
	}
	return false, fmt.Sprintf("outside backfill window %02d:%02d-%02d:%02d", w.start/60, w.start%60, w.end/60, w.end%60)
}

// ollamaGate pauses backfill while models other than ours hold more than
// maxForeign bytes on the target Ollama (someone else is using the GPU).
type ollamaGate struct {
	url        string
	ours       []string
	maxForeign int64
	http       *http.Client
}

// NewOllamaGate watches GET <url>/api/ps.
func NewOllamaGate(url string, ourModels []string, maxForeignGB float64) LoadGate {
	return &ollamaGate{url: strings.TrimRight(url, "/"), ours: ourModels, maxForeign: int64(maxForeignGB * (1 << 30)), http: &http.Client{Timeout: 5 * time.Second}}
}

func (g *ollamaGate) Name() string { return "ollama_load" }

func baseModel(name string) string { return strings.TrimSuffix(name, ":latest") }

func (g *ollamaGate) Allow(ctx context.Context, _ time.Time) (bool, string) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, g.url+"/api/ps", nil)
	resp, err := g.http.Do(req)
	if err != nil {
		return true, "ollama /api/ps unreachable; not yielding"
	}
	defer resp.Body.Close()
	var ps struct {
		Models []struct {
			Name     string `json:"name"`
			Size     int64  `json:"size"`
			SizeVRAM int64  `json:"size_vram"`
		} `json:"models"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&ps) != nil {
		return true, "ollama /api/ps unreadable; not yielding"
	}
	var foreign int64
	var names []string
	for _, m := range ps.Models {
		if slices.ContainsFunc(g.ours, func(o string) bool { return baseModel(o) == baseModel(m.Name) }) {
			continue
		}
		sz := m.SizeVRAM
		if sz == 0 {
			sz = m.Size
		}
		foreign += sz
		names = append(names, m.Name)
	}
	if foreign > g.maxForeign {
		return false, fmt.Sprintf("other models resident on ollama (%s, %.1f GB)", strings.Join(names, ", "), float64(foreign)/(1<<30))
	}
	return true, ""
}

// datawatchGate pauses backfill while any watched datawatch capacity pool
// (e.g. "node:<name>", "llm:<name>") is full or has waiters. The token needs
// the autonomous:read capability.
type datawatchGate struct {
	apiURL, token string
	pools         []string
	http          *http.Client
}

// NewDatawatchGate watches GET <apiURL>/api/capacity for pools.
func NewDatawatchGate(apiURL, token string, pools []string) LoadGate {
	return &datawatchGate{apiURL: strings.TrimRight(apiURL, "/"), token: token, pools: pools, http: &http.Client{Timeout: 5 * time.Second}}
}

func (g *datawatchGate) Name() string { return "datawatch_capacity" }

func (g *datawatchGate) Allow(ctx context.Context, _ time.Time) (bool, string) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, g.apiURL+"/api/capacity", nil)
	req.Header.Set("Authorization", "Bearer "+g.token)
	resp, err := g.http.Do(req)
	if err != nil {
		return true, "datawatch unreachable; not yielding"
	}
	defer resp.Body.Close()
	var c struct {
		Pools []struct {
			Name     string `json:"name"`
			Limit    int    `json:"limit"`
			External int    `json:"external"`
			Held     int    `json:"held"`
		} `json:"pools"`
		Waiting []struct {
			Pools []string `json:"pools"`
		} `json:"waiting"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(resp.Body).Decode(&c) != nil {
		return true, fmt.Sprintf("datawatch capacity unreadable (HTTP %d); not yielding", resp.StatusCode)
	}
	for _, p := range c.Pools {
		if !slices.Contains(g.pools, p.Name) {
			continue
		}
		if p.Limit > 0 && p.Held+p.External >= p.Limit {
			return false, fmt.Sprintf("datawatch pool %s is full (%d/%d)", p.Name, p.Held+p.External, p.Limit)
		}
	}
	for _, w := range c.Waiting {
		for _, p := range w.Pools {
			if slices.Contains(g.pools, p) {
				return false, fmt.Sprintf("datawatch work is waiting on pool %s", p)
			}
		}
	}
	return true, ""
}

// cachedGate memoises another gate's verdict for ttl, so a busy queue does
// not poll Ollama/datawatch on every batch.
type cachedGate struct {
	LoadGate
	ttl    time.Duration
	mu     gosync.Mutex
	at     time.Time
	ok     bool
	reason string
}

func cached(g LoadGate, ttl time.Duration) LoadGate { return &cachedGate{LoadGate: g, ttl: ttl} }

func (c *cachedGate) Allow(ctx context.Context, now time.Time) (bool, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && now.Sub(c.at) < c.ttl {
		return c.ok, c.reason
	}
	c.ok, c.reason = c.LoadGate.Allow(ctx, now)
	c.at = now
	return c.ok, c.reason
}

// rateLimiter is a token bucket refilled at perMinute; 0 = unlimited.
type rateLimiter struct {
	perMinute int
	mu        gosync.Mutex
	tokens    float64
	last      time.Time
}

func newRateLimiter(perMinute int) *rateLimiter {
	return &rateLimiter{perMinute: perMinute, tokens: float64(perMinute)}
}

// take returns how many of want may proceed now.
func (r *rateLimiter) take(now time.Time, want int) int {
	if r.perMinute <= 0 {
		return want
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.last.IsZero() {
		r.tokens += now.Sub(r.last).Minutes() * float64(r.perMinute)
		if r.tokens > float64(r.perMinute) {
			r.tokens = float64(r.perMinute)
		}
	}
	r.last = now
	n := min(want, int(r.tokens))
	r.tokens -= float64(n)
	return n
}
