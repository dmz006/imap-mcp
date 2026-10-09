package intel

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/dmz006/imap-mcp/internal/db"
	"github.com/dmz006/imap-mcp/internal/testutil/imaptest"
)

// edges returns "subject -pred-> object" → weight for one predicate.
func edges(t *testing.T, conn *sql.DB, pred string) map[string]int {
	t.Helper()
	rows, err := conn.Query(`SELECT s.name, o.name, r.weight FROM kg_relationships r
		JOIN kg_entities s ON s.id = r.subject_id JOIN kg_entities o ON o.id = r.object_id WHERE r.predicate = ?`, pred)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var s, o string
		var w int
		if err := rows.Scan(&s, &o, &w); err != nil {
			t.Fatal(err)
		}
		out[s+" -> "+o] = w
	}
	return out
}

func kgDB(t *testing.T) *db.DB {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(db.Options{Path: dir + "/imap.db"}, db.Options{Path: dir + "/cache.db"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func addOne(t *testing.T, d *db.DB, h Header, own map[string]bool) {
	t.Helper()
	tx, err := d.StateSQL().Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := newKGWriter(context.Background(), tx).addMessage(h, "me@example.com", own); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestAddMessageEdges(t *testing.T) {
	d := kgDB(t)
	own := map[string]bool{"me@example.com": true}
	now := time.Now()
	a := func(addr string) Address { return Address{Addr: addr} }

	// Incoming conversation, CC'd to a colleague: correspondent, org, cc pair, thread.
	addOne(t, d, Header{Date: now, MessageID: "m1@x", References: []string{"root@x"}, From: a("ann@acme.example"),
		To: []Address{a("me@example.com")}, Cc: []Address{a("bob@acme.example"), a("carl@gmail.com")}}, own)
	// Our reply to Ann: Ann corresponds again (weight 2).
	addOne(t, d, Header{Date: now.Add(time.Hour), MessageID: "m2@x", InReplyTo: "m1@x", From: a("me@example.com"),
		To: []Address{a("ann@acme.example")}}, own)
	// A newsletter: subscription only.
	addOne(t, d, Header{Date: now, MessageID: "n1@x", From: a("news@list.example"), To: []Address{a("me@example.com")}, List: true}, own)
	// A blast to 9 people: no cc_with clique, no thread edges.
	var many []Address
	for _, n := range []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"} {
		many = append(many, a(n+"@big.example"))
	}
	addOne(t, d, Header{Date: now, MessageID: "b1@x", From: a("boss@big.example"), To: many}, own)

	c := d.StateSQL()
	if got := edges(t, c, PredCorrespondsWith); got["ann@acme.example -> me@example.com"] != 2 || got["boss@big.example -> me@example.com"] != 1 {
		t.Errorf("corresponds_with = %v", got)
	}
	if got := edges(t, c, PredBelongsTo); got["ann@acme.example -> acme.example"] != 2 || got["bob@acme.example -> acme.example"] != 1 {
		t.Errorf("belongs_to = %v", got)
	}
	if got := edges(t, c, PredBelongsTo); got["carl@gmail.com -> gmail.com"] != 0 {
		t.Error("webmail domains are not organizations")
	}
	cc := edges(t, c, PredCcWith)
	if cc["ann@acme.example -> bob@acme.example"] != 1 || cc["ann@acme.example -> carl@gmail.com"] != 1 || len(cc) != 3 {
		t.Errorf("cc_with = %v (want 3 pairs among ann, bob, carl; none from the 10-person blast)", cc)
	}
	if got := edges(t, c, PredIsSubscription); got["news@list.example -> me@example.com"] != 1 || edges(t, c, PredCorrespondsWith)["news@list.example -> me@example.com"] != 0 {
		t.Errorf("subscription = %v", got)
	}
	thr := edges(t, c, PredParticipatesIn)
	if thr["ann@acme.example -> root@x"] != 1 || thr["ann@acme.example -> m1@x"] != 1 || thr["bob@acme.example -> root@x"] != 1 {
		t.Errorf("participates_in = %v", thr)
	}
}

func TestParseRelationsAndStrip(t *testing.T) {
	rels := parseRelations(`<think>x</think>{"relations":[{"subject":"Ann Lee","predicate":"Manages","object":"Bob"},` +
		`{"subject":"a","predicate":"deletes_mailbox","object":"b"},{"subject":"","predicate":"works_at","object":"Acme"},` +
		`{"subject":"thread","predicate":"deadline","object":"Contract renewal","due":"2026-11-01"},` +
		`{"subject":"x","predicate":"deadline","object":"Report","due":"soon"}]}`)
	if len(rels) != 3 || rels[0].Predicate != PredManages || rels[1].Due != "2026-11-01" || rels[2].Due != "" {
		t.Fatalf("relations = %+v", rels)
	}
	// The shape small models actually return for deadlines (seen live).
	r := parseRelations(`{"relations":[{"subject":"vendor contract renewal","predicate":"deadline","object":"2026-11-15"},` +
		`{"subject":"Dana","predicate":"manages","object":"Sam","due":"2026-11-15"}]}`)
	if len(r) != 2 || r[0].Object != "vendor contract renewal" || r[0].Due != "2026-11-15" || r[1].Due != "" {
		t.Errorf("normalised = %+v", r)
	}
	if r := parseRelations(`{"relations":[{"subject":"thread","predicate":"deadline","object":"...","due":"2026-11-15"}]}`); len(r) != 0 {
		t.Errorf("placeholder name kept: %+v", r)
	}
	if parseRelations("nothing here") != nil {
		t.Error("garbage must parse to nothing")
	}
	body := "Thanks, let's ship Friday.\n> earlier quoted secret\nOn Mon, Ann wrote:\n> more\n"
	if got := stripQuoted(body); got != "Thanks, let's ship Friday." {
		t.Errorf("stripQuoted = %q", got)
	}
	if got := cleanName("  Big\x00  Project\n" + strings.Repeat("x", 100)); len(got) > 80 || strings.ContainsAny(got, "\x00\n") {
		t.Errorf("cleanName = %q", got)
	}
}

// TestKGEndToEnd: the scan builds header edges once per message (copies and
// rescans included), cached tags add project/topic edges, the model adds
// relations from a cached body, and staleness is applied.
func TestKGEndToEnd(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	var prompts []string
	f.sc.classify = func(ctx context.Context, prompt string) (string, error) {
		prompts = append(prompts, prompt)
		if strings.Contains(prompt, "Extract relationships") {
			return `{"relations":[{"subject":"Alice","predicate":"manages","object":"Bob"},` +
				`{"subject":"thread","predicate":"deadline","object":"Q4 Budget","due":"2026-11-01"},` +
				`{"subject":"x","predicate":"exfiltrate","object":"y"}]}`, nil
		}
		return `{"role":"vendor"}`, nil
	}
	// Alice's message is cached, enriched, tagged and classified as conversation.
	res, err := f.d.Messages.Insert(ctx, &db.CachedMessage{Account: "test", Folder: "INBOX", UID: 1, MessageID: "a1@example.com",
		ThreadID: "a1@example.com", FromAddr: "alice@example.com", Subject: "Budget", Date: time.Now(), InternalDate: time.Now(),
		BodyText: "Bob reports to me now. Budget due Nov 1.\n> quoted old text\n", Flags: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.d.SQL().Exec(`UPDATE messages SET enrichment_status='done', hall='conversation', wing='Apollo', room='Budget' WHERE id=?`, res.ID); err != nil {
		t.Fatal(err)
	}

	if err := f.sc.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	c := f.d.StateSQL()
	if got := edges(t, c, PredCorrespondsWith)["alice@example.com -> "+imaptest.Username]; got != 2 {
		t.Errorf("alice corresponds_with weight = %d, want 2 (her mail + our reply; the Archive copy counts once)", got)
	}
	if got := edges(t, c, PredIsSubscription)["news@list.example.net -> "+imaptest.Username]; got != 3 {
		t.Errorf("newsletter subscription weight = %d", got)
	}
	if got := edges(t, c, PredWorksOn)["alice@example.com -> apollo"]; got != 1 {
		t.Errorf("works_on from tags = %v", edges(t, c, PredWorksOn))
	}
	if got := edges(t, c, PredDiscusses)["alice@example.com -> budget"]; got != 1 {
		t.Errorf("discusses = %v", edges(t, c, PredDiscusses))
	}
	if got := edges(t, c, PredManages)["Alice -> Bob"]; got != 1 {
		t.Errorf("model manages = %v", edges(t, c, PredManages))
	}
	var conf float64
	var props string
	c.QueryRow(`SELECT r.confidence, r.properties FROM kg_relationships r JOIN kg_entities o ON o.id=r.object_id WHERE r.predicate='deadline' AND o.name='q4 budget'`).Scan(&conf, &props) //nolint:errcheck
	if conf != confModel || !strings.Contains(props, "2026-11-01") {
		t.Errorf("deadline conf=%v props=%q", conf, props)
	}
	if len(edges(t, c, "exfiltrate")) != 0 {
		t.Error("an unknown predicate from the model was stored")
	}
	for _, p := range prompts {
		if strings.Contains(p, "quoted old text") {
			t.Error("quoted text reached the model")
		}
	}

	// Second tick: nothing is counted or extracted twice.
	n := len(prompts)
	if err := f.sc.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := edges(t, c, PredCorrespondsWith)["alice@example.com -> "+imaptest.Username]; got != 2 {
		t.Errorf("second tick changed weight to %d", got)
	}
	for _, p := range prompts[n:] {
		if strings.Contains(p, "Extract relationships") {
			t.Error("a body was sent to the model twice")
		}
	}

	// Staleness: old evidence becomes history, fresh evidence is current.
	c.Exec(`UPDATE kg_relationships SET last_seen = ? WHERE predicate = 'is_subscription'`, time.Now().Add(-400*24*time.Hour).Unix()) //nolint:errcheck
	if err := f.sc.markStale(ctx); err != nil {
		t.Fatal(err)
	}
	var stale, current int
	c.QueryRow(`SELECT count(*) FROM kg_relationships WHERE predicate='is_subscription' AND valid_to IS NOT NULL`).Scan(&stale) //nolint:errcheck
	c.QueryRow(`SELECT count(*) FROM kg_relationships WHERE predicate='corresponds_with' AND valid_to IS NULL`).Scan(&current)  //nolint:errcheck
	if stale == 0 || current == 0 {
		t.Errorf("stale=%d current=%d", stale, current)
	}
}

// TestKGRescanAfterUpgrade: profiles built without the graph (0.12), then the
// graph turned on with the scan restarted (what the upgrade migration does):
// edges are built for every message, and profile counts do not change.
func TestKGRescanAfterUpgrade(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	off := false
	f.sc.cfg.KG = &off
	if err := f.sc.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	c := f.d.StateSQL()
	before := f.sender(t, "alice@example.com")
	if len(edges(t, c, PredCorrespondsWith)) != 0 {
		t.Fatal("graph built while disabled")
	}
	f.sc.cfg.KG = nil
	if _, err := c.Exec(`UPDATE intel_scan SET last_uid = 0, completed_at = NULL`); err != nil {
		t.Fatal(err)
	}
	if err := f.sc.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := edges(t, c, PredCorrespondsWith)["alice@example.com -> "+imaptest.Username]; got != 2 {
		t.Errorf("rescan built weight %d, want 2", got)
	}
	if after := f.sender(t, "alice@example.com"); after.Received != before.Received || after.Sent != before.Sent || after.Replies != before.Replies {
		t.Errorf("rescan changed the profile: %+v → %+v", before, after)
	}
}
