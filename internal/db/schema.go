package db

// droppedCacheTables moved to imap.db in 0.12.0 (D19). They were never
// populated in cache.db, so dropping them loses nothing; children first.
var droppedCacheTables = []string{"anomalies", "kg_relationships", "kg_entities", "senders"}

// cacheSchema defines the disposable mail cache (cache.db, AGENT.md D1b):
// messages, FTS5, vectors, wing/room/hall tags, sync state and the enrichment
// queue. Every row can be rebuilt from IMAP.
// cacheSchemaVersion is stored in cache.db's user_version. A mismatch drops
// and recreates the cache (it is disposable); bump it on any change to a
// cache table's shape. Removing a table does not need a bump: list it in
// droppedCacheTables instead, so the cache is kept.
const cacheSchemaVersion = 3

const cacheSchema = `
-- ─── Messages ────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS messages (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    account         TEXT NOT NULL,
    folder          TEXT NOT NULL,
    uid             INTEGER NOT NULL,
    message_id      TEXT,               -- RFC 2822 Message-ID header
    thread_id       TEXT,               -- derived thread grouping key
    subject         TEXT,
    from_addr       TEXT NOT NULL,
    from_name       TEXT,
    to_addrs        TEXT,               -- JSON array
    cc_addrs        TEXT,               -- JSON array
    reply_to        TEXT,
    date            INTEGER NOT NULL,   -- unix timestamp (Date header, else INTERNALDATE)
    internal_date   INTEGER,            -- IMAP INTERNALDATE; drives the cache window
    flags           TEXT,               -- JSON array: \Seen \Answered \Flagged etc
    size            INTEGER,
    body_text       TEXT,
    body_html       TEXT,
    body_skipped    INTEGER DEFAULT 0,  -- 1 = over sync.max_message_mb, headers only
    has_attachments INTEGER DEFAULT 0,
    attachments     TEXT,               -- JSON array of {name, mime, size}

    -- datawatch memory patterns
    hall            TEXT DEFAULT 'unclassified', -- transactional|conversation|newsletter|notification|alert|personal
    wing            TEXT,               -- project/context label
    room            TEXT,               -- topic cluster

    -- enrichment
    enrichment_status TEXT DEFAULT 'pending', -- pending|processing|done|error
    enrichment_error  TEXT,
    enriched_at       INTEGER,

    -- sync metadata
    synced_at       INTEGER DEFAULT (unixepoch()),

    UNIQUE(account, folder, uid)
);

CREATE INDEX IF NOT EXISTS idx_messages_account_folder ON messages(account, folder);
CREATE INDEX IF NOT EXISTS idx_messages_from_addr      ON messages(from_addr);
CREATE INDEX IF NOT EXISTS idx_messages_date           ON messages(date DESC);
CREATE INDEX IF NOT EXISTS idx_messages_thread         ON messages(thread_id);
CREATE INDEX IF NOT EXISTS idx_messages_hall           ON messages(hall);
CREATE INDEX IF NOT EXISTS idx_messages_wing           ON messages(wing);
CREATE INDEX IF NOT EXISTS idx_messages_enrichment     ON messages(enrichment_status);
CREATE INDEX IF NOT EXISTS idx_messages_message_id     ON messages(message_id);

-- Full-text search over subject + body
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
    subject,
    body_text,
    from_addr,
    from_name,
    content='messages',
    content_rowid='id'
);

-- FTS sync triggers
CREATE TRIGGER IF NOT EXISTS messages_fts_insert AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts(rowid, subject, body_text, from_addr, from_name)
    VALUES (new.id, new.subject, new.body_text, new.from_addr, new.from_name);
END;
CREATE TRIGGER IF NOT EXISTS messages_fts_delete AFTER DELETE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, subject, body_text, from_addr, from_name)
    VALUES ('delete', old.id, old.subject, old.body_text, old.from_addr, old.from_name);
END;
CREATE TRIGGER IF NOT EXISTS messages_fts_update AFTER UPDATE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, subject, body_text, from_addr, from_name)
    VALUES ('delete', old.id, old.subject, old.body_text, old.from_addr, old.from_name);
    INSERT INTO messages_fts(rowid, subject, body_text, from_addr, from_name)
    VALUES (new.id, new.subject, new.body_text, new.from_addr, new.from_name);
END;

-- ─── Vectors ─────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS message_vectors (
    message_id  INTEGER PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
    vector      BLOB NOT NULL,          -- float32 array, little-endian
    model       TEXT NOT NULL,          -- embedding model name
    dims        INTEGER NOT NULL,       -- vector dimensions
    created_at  INTEGER DEFAULT (unixepoch())
);

-- ─── Folders ─────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS folders (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    account        TEXT NOT NULL,
    path           TEXT NOT NULL,
    delimiter      TEXT,
    attributes     TEXT,           -- JSON array
    last_synced    INTEGER,
    message_count  INTEGER DEFAULT 0,
    unseen_count   INTEGER DEFAULT 0,
    UNIQUE(account, path)
);

-- ─── Sync state ──────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS sync_state (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    account      TEXT NOT NULL,
    folder       TEXT NOT NULL,
    uid_validity INTEGER,
    highest_modseq INTEGER DEFAULT 0,  -- CONDSTORE; 0 = unknown/unsupported
    last_uid     INTEGER DEFAULT 0,
    last_synced  INTEGER,
    UNIQUE(account, folder)
);

-- ─── Enrichment queue ────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS enrichment_queue (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    message_id   INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    status       TEXT DEFAULT 'pending',  -- pending|processing|done|error
    lane         INTEGER NOT NULL DEFAULT 1, -- 0 = new mail, 1 = backfill (D11b)
    attempts     INTEGER DEFAULT 0,
    last_error   TEXT,
    queued_at    INTEGER DEFAULT (unixepoch()),
    processed_at INTEGER,
    UNIQUE(message_id)
);

CREATE INDEX IF NOT EXISTS idx_enrich_status ON enrichment_queue(status, lane, queued_at);
`

// stateSchema holds state that cannot be rebuilt from the cache: rules,
// webhooks, inbound replay nonces and the intelligence tables (sender
// profiles, knowledge graph, anomalies, the D28 index and scan progress). It
// lives in imap.db (AGENT.md D1b, D19).
const stateSchema = `
-- ─── Webhooks ────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS webhooks (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    url        TEXT NOT NULL,
    events     TEXT NOT NULL,  -- JSON array of EventType strings
    secret     TEXT,
    active     INTEGER DEFAULT 1,
    created_at INTEGER DEFAULT (unixepoch()),
    last_fired INTEGER,
    fail_count INTEGER DEFAULT 0,
    payload    TEXT DEFAULT 'metadata' -- D46: metadata | full | comma-separated fields
);

-- Durable webhook outbox (AGENT.md D16): one row per (webhook, event),
-- retried with backoff until delivered or out of attempts. payload is
-- metadata only (identifiers, counts, flags), never message content.
CREATE TABLE IF NOT EXISTS webhook_deliveries (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    webhook_id   INTEGER NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
    delivery_id  TEXT NOT NULL UNIQUE,
    event        TEXT NOT NULL,
    payload      TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'pending', -- pending|delivered|failed
    attempts     INTEGER NOT NULL DEFAULT 0,
    next_attempt INTEGER NOT NULL DEFAULT (unixepoch()),
    last_status  INTEGER,
    last_error   TEXT,
    created_at   INTEGER NOT NULL DEFAULT (unixepoch()),
    done_at      INTEGER
);

CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_due ON webhook_deliveries(status, next_attempt);
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_hook ON webhook_deliveries(webhook_id, id);

-- ─── Rules ───────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS rules (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL UNIQUE,
    description TEXT,
    conditions  TEXT NOT NULL,  -- JSON: [{field, op, value}]
    actions     TEXT NOT NULL,  -- JSON: [{type, params}]
    active      INTEGER DEFAULT 1,
    priority    INTEGER DEFAULT 100,
    run_count   INTEGER DEFAULT 0,
    created_at  INTEGER DEFAULT (unixepoch()),
    updated_at  INTEGER DEFAULT (unixepoch())
);

-- Replay protection for the inbound command channel: each (account, nonce)
-- may be honored at most once.
CREATE TABLE IF NOT EXISTS inbound_nonces (
    account   TEXT NOT NULL,
    nonce     TEXT NOT NULL,
    cmd_ts    INTEGER,
    seen_at   INTEGER DEFAULT (unixepoch()),
    PRIMARY KEY (account, nonce)
);

-- ─── Intelligence (AGENT.md D19, D20, D28) ──────────────────────────────────
-- Durable: built from a header scan of all history plus enriched cache
-- signals, so it survives the cache window and cache rebuilds.

CREATE TABLE IF NOT EXISTS senders (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    address          TEXT UNIQUE NOT NULL,
    name             TEXT,
    domain           TEXT,
    role             TEXT DEFAULT 'unknown', -- colleague|vendor|newsletter|bot|personal|unknown
    role_source      TEXT,                   -- signal:<kind> | hall | llm
    role_checked_at  INTEGER,
    first_seen       INTEGER,
    last_seen        INTEGER,
    message_count    INTEGER DEFAULT 0,     -- messages received from them
    sent_count       INTEGER DEFAULT 0,     -- messages we sent to them
    list_count       INTEGER DEFAULT 0,     -- received with List-Id / List-Unsubscribe
    bulk_count       INTEGER DEFAULT 0,     -- received with Precedence: bulk/list/junk
    auto_count       INTEGER DEFAULT 0,     -- received with Auto-Submitted (not "no")
    dkim_pass        INTEGER DEFAULT 0,
    dkim_fail        INTEGER DEFAULT 0,
    dmarc_pass       INTEGER DEFAULT 0,
    dmarc_fail       INTEGER DEFAULT 0,
    reply_count      INTEGER DEFAULT 0,     -- our replies paired to their messages
    reply_total_secs INTEGER DEFAULT 0,
    avg_reply_time   INTEGER,               -- seconds (reply_total_secs / reply_count)
    anomaly_score    REAL DEFAULT 0.0,
    trusted          INTEGER DEFAULT 0,     -- released from a new-sender hold (D30)
    profile_json     TEXT,
    dirty            INTEGER DEFAULT 1,     -- role needs recomputing
    updated_at       INTEGER DEFAULT (unixepoch())
);

CREATE INDEX IF NOT EXISTS idx_senders_domain ON senders(domain);
CREATE INDEX IF NOT EXISTS idx_senders_role   ON senders(role);
CREATE INDEX IF NOT EXISTS idx_senders_dirty  ON senders(dirty) WHERE dirty = 1;

-- One row per message seen by the header scan (D28): hashes only, no
-- subjects, bodies or addresses. De-duplicates across folders and labels and
-- pairs our replies with the message they answer.
CREATE TABLE IF NOT EXISTS intel_messages (
    account    TEXT NOT NULL,
    msg_hash   INTEGER NOT NULL,  -- Message-ID hash
    date       INTEGER NOT NULL,
    sender_id  INTEGER,           -- senders.id for incoming mail
    outgoing   INTEGER NOT NULL DEFAULT 0,
    reply_hash INTEGER,           -- In-Reply-To hash (outgoing only)
    paired     INTEGER NOT NULL DEFAULT 0,
    kg_done      INTEGER NOT NULL DEFAULT 0, -- header edges added to the knowledge graph (P3)
    kg_tags_done INTEGER NOT NULL DEFAULT 0, -- wing/room edges added from the cache
    kg_llm_done  INTEGER NOT NULL DEFAULT 0, -- body read by the extraction model
    folder       TEXT,                       -- where the scan last saw it (D34)
    PRIMARY KEY (account, msg_hash)
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS idx_intel_messages_unpaired ON intel_messages(account, reply_hash) WHERE outgoing = 1 AND paired = 0;

-- Header-scan progress per folder.
CREATE TABLE IF NOT EXISTS intel_scan (
    account      TEXT NOT NULL,
    folder       TEXT NOT NULL,
    uidvalidity  INTEGER NOT NULL DEFAULT 0,
    last_uid     INTEGER NOT NULL DEFAULT 0,
    scanned      INTEGER NOT NULL DEFAULT 0,  -- messages processed in this folder
    completed_at INTEGER,                     -- first time the folder was fully scanned
    rescan_until INTEGER,                     -- one-time rescan in progress until last_uid reaches this (0.16)
    updated_at   INTEGER DEFAULT (unixepoch()),
    PRIMARY KEY (account, folder)
);

-- reply_threads: the latest message of each person-to-person conversation
-- (D32), for needs_reply and awaiting_reply. Filled by the header scan from
-- the whole history. A reply clears an item, as do \Answered and an explicit
-- dismiss (D33); a later message in the thread re-opens a dismissed one.
CREATE TABLE IF NOT EXISTS reply_threads (
    account      TEXT NOT NULL,
    thread_hash  INTEGER NOT NULL,           -- D28 key of the thread root
    thread_id    TEXT,                       -- the thread root, as get_thread takes it
    last_hash    INTEGER NOT NULL,           -- D28 key of the latest message
    last_date    INTEGER NOT NULL,
    outgoing     INTEGER NOT NULL DEFAULT 0, -- the owner sent the latest message
    direct       INTEGER NOT NULL DEFAULT 0, -- incoming and addressed to the owner (To/Cc)
    counterpart  TEXT,                       -- incoming: the sender; outgoing: the first recipient
    subject      TEXT,
    message_ref  TEXT,                       -- Message-ID of the latest message
    folder       TEXT,                       -- where the scan last saw it
    uid          INTEGER,
    answered     INTEGER NOT NULL DEFAULT 0, -- the latest message carries \Answered
    dismissed_hash INTEGER,                  -- dismiss_reply: last_hash when dismissed; a newer message re-opens it
    dismissed_at   INTEGER,
    PRIMARY KEY (account, thread_hash)
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS idx_reply_threads_open ON reply_threads(account, outgoing, last_date);

-- intel_locscan: progress of the location-only pass over Trash and Junk
-- (D34): Message-ID hashes and folder only, never profiles or graph edges.
CREATE TABLE IF NOT EXISTS intel_locscan (
    account     TEXT NOT NULL,
    folder      TEXT NOT NULL,
    uidvalidity INTEGER NOT NULL DEFAULT 0,
    last_uid    INTEGER NOT NULL DEFAULT 0,
    updated_at  INTEGER DEFAULT (unixepoch()),
    PRIMARY KEY (account, folder)
);

-- rule_moves: messages a rule moved or trashed (D34), so they never count as
-- the owner's own discards. Pruned after a year.
CREATE TABLE IF NOT EXISTS rule_moves (
    account  TEXT NOT NULL,
    msg_hash INTEGER NOT NULL,
    rule_id  INTEGER,
    dest     TEXT,
    moved_at INTEGER NOT NULL,
    PRIMARY KEY (account, msg_hash)
) WITHOUT ROWID;

-- learn_state: what learning from moves did with each sender or domain
-- (D35, D36, D47): suggested, created (rule_id) or dismissed (never again).
CREATE TABLE IF NOT EXISTS learn_state (
    account    TEXT NOT NULL,
    target     TEXT NOT NULL,  -- an address, or @domain
    status     TEXT NOT NULL,  -- suggested | created | dismissed
    rule_id    INTEGER,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (account, target)
);

-- identities: addresses that may be the owner's own (D50). Candidates are
-- detected from the owner's display name and address forms; the owner
-- confirms or rejects them. Confirmed ones count as the owner everywhere.
CREATE TABLE IF NOT EXISTS identities (
    address    TEXT PRIMARY KEY,  -- lower-case address or @domain
    status     TEXT NOT NULL,     -- candidate | confirmed | rejected | config
    evidence   TEXT,
    applied_at INTEGER,           -- when the stored history was rewritten for it
    updated_at INTEGER NOT NULL
);

-- owner_names: display names the owner sends under, counted from outgoing
-- mail; identity detection looks for them on other addresses (D50).
CREATE TABLE IF NOT EXISTS owner_names (
    name  TEXT PRIMARY KEY,  -- lower-case
    count INTEGER NOT NULL DEFAULT 0
);

-- setup_findings: setup-check findings already reported in a digest (D52),
-- so the digest's Setup section shows only new ones.
CREATE TABLE IF NOT EXISTS setup_findings (
    id          TEXT PRIMARY KEY,  -- finding id plus account
    first_seen  INTEGER NOT NULL,
    digested_at INTEGER
);

-- digest_log: when each account's daily digest was last sent (D31).
CREATE TABLE IF NOT EXISTS digest_log (
    account TEXT PRIMARY KEY,
    sent_at INTEGER NOT NULL
);

-- held_messages: mail a new_sender rule held (D30), for release detection
-- and the daily digest (D31). Rows are pruned after 90 days.
CREATE TABLE IF NOT EXISTS held_messages (
    account      TEXT NOT NULL,
    msg_hash     INTEGER NOT NULL,  -- D28 key of the Message-ID
    message_ref  TEXT,              -- the Message-ID
    sender       TEXT,
    subject      TEXT,              -- first 200 bytes, for the digest
    reasons      TEXT,              -- why it was held
    folder       TEXT,              -- where it was moved
    held_at      INTEGER NOT NULL,
    released_at  INTEGER,           -- found back in the inbox: the sender is trusted
    digested_at  INTEGER,
    PRIMARY KEY (account, msg_hash)
);

CREATE TABLE IF NOT EXISTS kg_entities (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    entity_type    TEXT NOT NULL,   -- person|organization|topic|project|thread
    name           TEXT NOT NULL,
    properties     TEXT,            -- JSON
    created_at     INTEGER DEFAULT (unixepoch()),
    UNIQUE(entity_type, name)
);

CREATE TABLE IF NOT EXISTS kg_relationships (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    subject_id  INTEGER NOT NULL REFERENCES kg_entities(id) ON DELETE CASCADE,
    predicate   TEXT NOT NULL,      -- manages|reports_to|belongs_to|is_subscription|sent_to etc
    object_id   INTEGER NOT NULL REFERENCES kg_entities(id) ON DELETE CASCADE,
    valid_from  INTEGER,            -- unix timestamp, null = always
    valid_to    INTEGER,            -- null = still valid
    confidence  REAL DEFAULT 1.0,
    properties  TEXT,               -- JSON
    weight      INTEGER NOT NULL DEFAULT 1, -- messages supporting the edge
    last_seen   INTEGER,            -- latest evidence; valid_to is set from it when stale
    created_at  INTEGER DEFAULT (unixepoch())
);

CREATE INDEX IF NOT EXISTS idx_kg_rel_subject   ON kg_relationships(subject_id);
CREATE INDEX IF NOT EXISTS idx_kg_rel_object    ON kg_relationships(object_id);
CREATE INDEX IF NOT EXISTS idx_kg_rel_predicate ON kg_relationships(predicate);

CREATE TABLE IF NOT EXISTS anomalies (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    account      TEXT NOT NULL,
    message_id   INTEGER,           -- cache row id when the message is cached (no FK: other file)
    sender       TEXT,
    anomaly_type TEXT NOT NULL,     -- see AGENT.md D22
    description  TEXT,
    severity     TEXT DEFAULT 'low', -- low|medium|high
    detected_at  INTEGER DEFAULT (unixepoch()),
    resolved     INTEGER DEFAULT 0,
    resolved_at  INTEGER,
    folder       TEXT,              -- per-message anomalies: where the message was found
    uid          INTEGER,
    message_ref  TEXT,              -- its Message-ID (get_thread, search)
    details      TEXT               -- JSON: the numbers behind the finding
);

CREATE INDEX IF NOT EXISTS idx_anomalies_account ON anomalies(account);
CREATE INDEX IF NOT EXISTS idx_anomalies_sender  ON anomalies(sender);
CREATE INDEX IF NOT EXISTS idx_anomalies_type    ON anomalies(anomaly_type);
`
