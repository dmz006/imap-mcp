package intel

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/dmz006/imap-mcp/internal/bus"
	"github.com/dmz006/imap-mcp/internal/testutil/imaptest"
)

func anomalyTypes(t *testing.T, f *fixture) map[string]int {
	t.Helper()
	rows, err := f.d.StateSQL().Query(`SELECT anomaly_type, count(*) FROM anomalies GROUP BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var typ string
		var n int
		rows.Scan(&typ, &n) //nolint:errcheck
		out[typ] = n
	}
	return out
}

func TestEditDistanceAndLookalike(t *testing.T) {
	if editDistance("example.com", "exarnple.com") != 2 || editDistance("abc", "abc") != 0 {
		t.Error("editDistance")
	}
	d := &detector{corrList: []string{"example.com", "acme-supplies.org"}}
	for dom, want := range map[string]string{
		"examp1e.com":      "example.com",       // 1 → l
		"exarnple.com":     "example.com",       // rn → m
		"exampel.com":      "example.com",       // transposition (distance 2, length 11)
		"acme-suplies.org": "acme-supplies.org", // distance 1
		"example.org":      "",                  // distance 3
		"unrelated.net":    "",
		"ex.io":            "", // too short to judge
	} {
		if got := d.lookalike(dom); got != want {
			t.Errorf("lookalike(%s) = %q, want %q", dom, got, want)
		}
	}
}

// TestPerMessageAnomalies: nothing is flagged while the history scan is
// incomplete; afterwards recent mail is checked, each finding is stored once
// with the message's location, and anomaly.detected carries no addresses.
func TestPerMessageAnomalies(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.sc.cfg.AnomalyLookbackDays = 30 // the fixture's history is 10 days old
	f.sc.cfg.AnomalyAuthMinPasses = 1
	b := bus.New()
	var events []bus.Event
	b.Subscribe(bus.EventAnomalyDetected, func(e bus.Event) { events = append(events, e) })
	f.sc.SetBus(b)

	// First tick: the history scan completes in this tick, so nothing is flagged
	// (otherwise every recent first-seen message would be a "new sender").
	if err := f.sc.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := anomalyTypes(t, f); len(got) != 0 {
		t.Fatalf("anomalies during the first scan: %v", got)
	}

	me := imaptest.Username
	now := time.Now()
	f.d.StateSQL().Exec(`UPDATE senders SET message_count = 5 WHERE address = 'alice@example.com'`) //nolint:errcheck
	f.srv.Append(t, "INBOX", msg("x1@new.example", "Stranger <stranger@new.example>", me, ""), now)
	f.srv.Append(t, "INBOX", msg("x2@example.com", "alice@example.com", me, "Authentication-Results: mx.example.com; dkim=fail; dmarc=fail\r\n"), now)
	f.srv.Append(t, "INBOX", msg("x3@examp1e.com", "billing@examp1e.com", me, ""), now)
	f.srv.Append(t, "INBOX", msg("x4@example.com", "alice@example.com", me, "Reply-To: alice@evil.example\r\n"), now)
	f.srv.Append(t, "INBOX", msg("x5@old.example", "old@old.example", me, ""), now.Add(-60*24*time.Hour)) // outside lookback
	if err := f.sc.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	got := anomalyTypes(t, f)
	want := map[string]int{AnomNewSender: 2, AnomAuthFailure: 1, AnomLookalikeDomain: 1, AnomReplyToMismatch: 1}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %d, want %d (all: %v)", k, got[k], v, got)
		}
	}
	var folder, ref, details string
	var uid int
	f.d.StateSQL().QueryRow(`SELECT folder, uid, message_ref, details FROM anomalies WHERE anomaly_type = 'auth_failure'`).Scan(&folder, &uid, &ref, &details) //nolint:errcheck
	if folder != "INBOX" || uid == 0 || ref != "x2@example.com" || !json.Valid([]byte(details)) {
		t.Errorf("auth_failure location = %s/%d ref=%s details=%s", folder, uid, ref, details)
	}
	if len(events) != 5 {
		var ids []any
		for _, e := range events {
			ids = append(ids, e.Payload)
		}
		t.Fatalf("events = %d: %v; rows %v", len(events), ids, got)
	}
	for _, e := range events {
		p, _ := json.Marshal(e.Payload)
		var m map[string]any
		json.Unmarshal(p, &m) //nolint:errcheck
		if len(m) != 3 || m["id"] == nil || m["type"] == nil || m["severity"] == nil {
			t.Errorf("payload must be id/type/severity only: %s", p)
		}
	}
	var score float64
	f.d.StateSQL().QueryRow(`SELECT anomaly_score FROM senders WHERE address = 'alice@example.com'`).Scan(&score) //nolint:errcheck
	if score != 1.5 {                                                                                             // auth_failure (high 1.0) + reply_to_mismatch (medium 0.5)
		t.Errorf("alice anomaly_score = %v", score)
	}

	// A third tick finds nothing new: findings are stored once per message.
	if err := f.sc.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if again := anomalyTypes(t, f); again[AnomNewSender] != 2 || len(events) != 5 {
		t.Errorf("re-flagged: %v, %d events", again, len(events))
	}
}

// TestPeriodicAnomalies: silence crosses its threshold (and ends when mail
// returns); a 24-hour burst is a volume spike; ancient silences are ignored.
func TestPeriodicAnomalies(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	c := f.d.StateSQL()
	day := int64(24 * 3600)
	now := time.Now().Unix()
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := c.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	// Carol wrote every ~5 days for 120 days, then stopped 40 days ago:
	// threshold max(30, 3x5=15) = 30 days, so 40 days silent is reported.
	// Dan stopped 400 days ago: long past, not news.
	for i, who := range []struct {
		addr   string
		lastIn int64
	}{{"carol@example.org", now - 40*day}, {"dan@example.org", now - 400*day}} {
		exec(`INSERT INTO senders(id, address, role, message_count, first_seen, dirty) VALUES(?,?,'personal',25,?,0)`, 100+i, who.addr, who.lastIn-120*day)
		for k := 0; k < 25; k++ {
			exec(`INSERT INTO intel_messages(account, msg_hash, date, sender_id, outgoing) VALUES('test',?,?,?,0)`,
				int64(1000*(i+1)+k), who.lastIn-int64(k)*5*day, 100+i)
		}
	}
	// Spammy sends 30 in the last day after a quiet year.
	exec(`INSERT INTO senders(id, address, role, message_count, first_seen, dirty) VALUES(200,'spammy@example.net','vendor',40,?,0)`, now-365*day)
	for k := 0; k < 30; k++ {
		exec(`INSERT INTO intel_messages(account, msg_hash, date, sender_id, outgoing) VALUES('test',?,?,200,0)`, int64(5000+k), now-int64(k)*60)
	}
	d := &detector{active: map[string]bool{"test": true}}
	f.sc.cfg.AnomalySilenceMinMessages, f.sc.cfg.AnomalySilenceMinDays = 20, 30
	f.sc.cfg.AnomalySpikeMin, f.sc.cfg.AnomalySpikeFactor = 10, 5
	found, err := f.sc.periodic(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if len(found["test"]) != 2 {
		t.Fatalf("found = %v", found)
	}
	got := anomalyTypes(t, f)
	if got[AnomSilence] != 1 || got[AnomVolumeSpike] != 1 {
		t.Fatalf("types = %v", got)
	}
	var who string
	c.QueryRow(`SELECT sender FROM anomalies WHERE anomaly_type = 'silence'`).Scan(&who) //nolint:errcheck
	if who != "carol@example.org" {
		t.Errorf("silence for %s", who)
	}
	// Running again does not duplicate open findings.
	if found, _ := f.sc.periodic(ctx, d); len(found["test"]) != 0 {
		t.Errorf("duplicated: %v", found)
	}
	// Carol writes again: the silence is resolved.
	exec(`INSERT INTO intel_messages(account, msg_hash, date, sender_id, outgoing) VALUES('test',9999,?,100,0)`, now)
	if _, err := f.sc.periodic(ctx, d); err != nil {
		t.Fatal(err)
	}
	var resolved int
	c.QueryRow(`SELECT resolved FROM anomalies WHERE anomaly_type = 'silence'`).Scan(&resolved) //nolint:errcheck
	if resolved != 1 {
		t.Error("silence not resolved when mail returned")
	}
}

func TestAnomaliesDisabled(t *testing.T) {
	f := newFixture(t)
	off := false
	f.sc.cfg.Anomalies = &off
	if d, err := f.sc.prepareDetector(context.Background(), []string{"test"}); err != nil || d != nil {
		t.Fatalf("detector = %v, %v", d, err)
	}
}
