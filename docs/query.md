# Query API

`POST /api/query` answers ad-hoc questions about the local mail cache and the
sender profiles with a JSON query (AGENT.md D17). It never accepts SQL. Views, fields, operators and
aggregate functions come from fixed allowlists, and every value is sent as a
bound parameter. The query runs read-only with a 10-second timeout. It needs a
token with the `admin` scope.

`GET /api/query` lists the views, their fields and types, the operators and
the aggregate functions.

## Shape

```json
{
  "view": "messages",
  "fields": ["uid", "date", "from_addr", "subject"],
  "where": [
    {"field": "date", "op": "gte", "value": "-7d"},
    {"field": "seen", "op": "eq", "value": false}
  ],
  "order_by": [{"field": "date", "desc": true}],
  "limit": 50,
  "offset": 0
}
```

The response looks like this:

```json
{"view": "messages", "columns": ["uid", "date", "from_addr", "subject"],
 "rows": [[4211, "2026-10-08T09:12:00Z", "billing@example.com", "Invoice"]],
 "count": 1, "truncated": false}
```

- **`view`:** `messages`, `senders`, `anomalies` or `kg` (knowledge-graph
  relationships, with subject and object names).
  `messages` reads the cache (the sync window). `senders`, `anomalies` and
  `kg` read `imap.db`, which covers all history ([intelligence.md](intelligence.md)).
  > **Currently empty:** nothing detects anomalies yet, so the `anomalies`
  > view returns no rows. `senders` and `kg` are filled by the header
  > scanner. See [known-limitations.md](known-limitations.md).
- **`fields`:** columns to return. If omitted, a default set is returned. For
  `messages`, the default never includes bodies: `body_text` and `body_html`
  are returned only when named.
- **`where`:** filters, all ANDed. Operators:
  - `eq`, `ne`, `lt`, `lte`, `gt`, `gte`;
  - `in` and `nin` (a list of up to 500 values);
  - `contains` and `prefix` (text only; `%` and `_` are literal);
  - `is_null` and `not_null`.
- **Times** are returned as RFC 3339 UTC. In filters they accept RFC 3339,
  `YYYY-MM-DD`, Unix seconds, or a relative offset (`-30d`, `-12h`, `-15m`).
- **Booleans:** `seen`, `flagged`, `answered`, `has_attachments` and
  `resolved` take `true` or `false`, with `eq` and `ne` only.
- **`group_by`** plus **`aggregate`** summarise instead of listing rows. You
  can't combine them with `fields`.
  - Aggregates are `count` (no field means rows), `count_distinct`, `min`,
    `max`, `sum` and `avg`.
  - Each takes an optional `as` alias: lowercase letters, digits and `_`.
- **`order_by`** may name only returned columns: fields, group-by fields or
  aggregate aliases.
- **`limit`** defaults to 100, with a maximum of 1000. `truncated: true` means
  more rows matched.

## Examples

Top sender domains over the last 30 days:

```json
{"view": "messages",
 "where": [{"field": "date", "op": "gte", "value": "-30d"}],
 "group_by": ["from_domain"],
 "aggregate": [{"fn": "count", "as": "n"}, {"fn": "max", "field": "date", "as": "latest"}],
 "order_by": [{"field": "n", "desc": true}],
 "limit": 20}
```

Unread, flagged mail per folder:

```json
{"view": "messages",
 "where": [{"field": "seen", "op": "eq", "value": false},
           {"field": "flagged", "op": "eq", "value": true}],
 "group_by": ["account", "folder"],
 "aggregate": [{"fn": "count", "as": "n"}]}
```

The next examples use the intelligence views. `senders` and `kg` return data;
the `anomalies` example returns no rows until anomaly detection is built.

Senders you have never replied to, ranked by volume:

```json
{"view": "senders",
 "fields": ["address", "message_count", "last_seen"],
 "where": [{"field": "sent_count", "op": "eq", "value": 0}],
 "order_by": [{"field": "message_count", "desc": true}]}
```

Senders whose mail sometimes fails DMARC, worst first (a P4 anomaly baseline):

```json
{"view": "senders",
 "fields": ["address", "role", "dmarc_pass", "dmarc_fail"],
 "where": [{"field": "dmarc_fail", "op": "gt", "value": 0}, {"field": "dmarc_pass", "op": "gt", "value": 0}],
 "order_by": [{"field": "dmarc_fail", "desc": true}], "limit": 20}
```

Roles across all senders:

```json
{"view": "senders", "group_by": ["role", "role_source"], "aggregate": [{"fn": "count", "as": "n"}],
 "order_by": [{"field": "n", "desc": true}]}
```

Open high-severity anomalies:

```json
{"view": "anomalies",
 "where": [{"field": "resolved", "op": "eq", "value": false},
           {"field": "severity", "op": "eq", "value": "high"}]}
```

Current relationships for an organization (`belongs_to` objects are
lower-case domains):

```json
{"view": "kg",
 "where": [{"field": "object", "op": "eq", "value": "example.com"},
           {"field": "current", "op": "eq", "value": true}],
 "order_by": [{"field": "weight", "desc": true}]}
```

Your strongest correspondents, and when you last heard from them:

```json
{"view": "kg", "fields": ["subject", "weight", "last_seen"],
 "where": [{"field": "predicate", "op": "eq", "value": "corresponds_with"}],
 "order_by": [{"field": "weight", "desc": true}], "limit": 20}
```

```bash
curl -sS -X POST http://127.0.0.1:8765/api/query \
  -H "Authorization: Bearer $IMAP_MCP_ADMIN_TOKEN" \
  -H "Content-Type: application/json" \
  -d @query.json
```

The cache holds only the synced window (default 30 days; see `sync.window_days`),
so queries cover that window, not the whole mailbox.
