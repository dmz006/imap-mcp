package db

// schema defines all tables, indexes, FTS5 virtual tables, and the
// datawatch-inspired memory patterns: wing/room/hall tagging, temporal KG,
// sender profiles, and anomaly episodic log.
const schema = `
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
    date            INTEGER NOT NULL,   -- unix timestamp
    flags           TEXT,               -- JSON array: \Seen \Answered \Flagged etc
    size            INTEGER,
    body_text       TEXT,
    body_html       TEXT,
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

-- ─── Senders (entity profiles) ───────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS senders (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    address          TEXT UNIQUE NOT NULL,
    name             TEXT,
    domain           TEXT,
    role             TEXT DEFAULT 'unknown', -- colleague|vendor|newsletter|bot|personal|unknown
    first_seen       INTEGER,
    last_seen        INTEGER,
    message_count    INTEGER DEFAULT 0,
    sent_count       INTEGER DEFAULT 0,     -- messages we sent to them
    avg_reply_time   INTEGER,               -- seconds
    anomaly_score    REAL DEFAULT 0.0,
    profile_json     TEXT,                  -- enriched profile blob
    updated_at       INTEGER DEFAULT (unixepoch())
);

CREATE INDEX IF NOT EXISTS idx_senders_domain ON senders(domain);
CREATE INDEX IF NOT EXISTS idx_senders_role   ON senders(role);

-- ─── Temporal Knowledge Graph ─────────────────────────────────────────────────

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
    created_at  INTEGER DEFAULT (unixepoch())
);

CREATE INDEX IF NOT EXISTS idx_kg_rel_subject   ON kg_relationships(subject_id);
CREATE INDEX IF NOT EXISTS idx_kg_rel_object    ON kg_relationships(object_id);
CREATE INDEX IF NOT EXISTS idx_kg_rel_predicate ON kg_relationships(predicate);

-- ─── Anomalies (episodic pattern log) ────────────────────────────────────────

CREATE TABLE IF NOT EXISTS anomalies (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    account      TEXT NOT NULL,
    message_id   INTEGER REFERENCES messages(id) ON DELETE SET NULL,
    sender       TEXT,
    anomaly_type TEXT NOT NULL,  -- behavior_change|silence|reply_spike|new_sender|role_shift
    description  TEXT,
    severity     TEXT DEFAULT 'low', -- low|medium|high
    detected_at  INTEGER DEFAULT (unixepoch()),
    resolved     INTEGER DEFAULT 0,
    resolved_at  INTEGER
);

CREATE INDEX IF NOT EXISTS idx_anomalies_account ON anomalies(account);
CREATE INDEX IF NOT EXISTS idx_anomalies_sender  ON anomalies(sender);
CREATE INDEX IF NOT EXISTS idx_anomalies_type    ON anomalies(anomaly_type);

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
    last_uid     INTEGER DEFAULT 0,
    last_synced  INTEGER,
    UNIQUE(account, folder)
);

-- ─── Enrichment queue ────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS enrichment_queue (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    message_id   INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    status       TEXT DEFAULT 'pending',  -- pending|processing|done|error
    attempts     INTEGER DEFAULT 0,
    last_error   TEXT,
    queued_at    INTEGER DEFAULT (unixepoch()),
    processed_at INTEGER,
    UNIQUE(message_id)
);

CREATE INDEX IF NOT EXISTS idx_enrich_status ON enrichment_queue(status, queued_at);

-- ─── Webhooks ────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS webhooks (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    url        TEXT NOT NULL,
    events     TEXT NOT NULL,  -- JSON array of EventType strings
    secret     TEXT,
    active     INTEGER DEFAULT 1,
    created_at INTEGER DEFAULT (unixepoch()),
    last_fired INTEGER,
    fail_count INTEGER DEFAULT 0
);

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
`
