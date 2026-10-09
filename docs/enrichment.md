# Enrichment

The enrichment pipeline processes cached messages in the background. Each
message gets two things:

1. **An embedding.** This vector powers `semantic_search` and
   `POST /api/search/semantic`.
2. **A classification.** This adds `hall`, `wing` and `room` tags, which the
   query DSL and the search results expose.

The pipeline works only on mail in the [sync cache](sync-cache.md), so mail
outside the sync window is never enriched. It runs in `imap-mcp serve` and in
stdio mode, and it is on by default. Set `enrichment.enabled: false` to turn it
off.

## What happens to a message

The syncer caches a message and puts it on the **enrichment queue**, unless
another copy with the same Message-ID is already queued. Every 10 seconds the
pipeline takes a batch of up to `batch_size` messages (default 20) and
processes each one:

1. **Embed.** The input is the subject plus the first 500 characters of the
   text body. If the message was cached headers-only, the input is the subject
   alone.
2. **Classify.** The prompt holds the sender, the subject and the first 300
   characters of the body. It asks the model for JSON in this form:
   `{"hall": "transactional|conversation|newsletter|notification|alert|personal", "wing": "...", "room": "..."}`.
   The pipeline takes the first `{` through the last `}` of the reply. If that
   text does not parse, the tags stay empty, and the message still counts as
   done.
3. **Store.** The vector goes into `message_vectors` and the tags into
   `messages`. The message is marked `done`, and `enrichment.done` is
   published.

## Providers

### Embeddings: Ollama

Embeddings always go straight to an Ollama `POST /api/embeddings`.

| Setting | Default |
|---------|---------|
| `enrichment.embed.url` | `enrichment.ollama_url` (`http://localhost:11434`) |
| `enrichment.embed.model` | `enrichment.embed_model` (`nomic-embed-text`) |

Semantic search embeds your query with the same embedder. If you change the
embedding model, vectors from the old and new models are no longer
comparable. Re-enrich with `cache_sweep { all: true, dry_run: false }`.

### Classification: Ollama (default)

The default provider calls an Ollama `POST /api/generate` (non-streaming).

| Setting | Default |
|---------|---------|
| `enrichment.classify.provider` | `ollama` |
| `enrichment.classify.url` | `enrichment.ollama_url` |
| `enrichment.classify.model` | `enrichment.llm_model` (`qwen3:1.7b`) |

### Classification: datawatch LLM proxy (not usable yet)

With `classify.provider: datawatch`, the pipeline sends
`POST <datawatch.api_url>/api/proxy/llm/<classify.datawatch_llm>` with
`{"prompt": ...}`. The proxy adds registry routing and node failover. Config
validation requires a `datawatch` block with `api_url` and a value for
`classify.datawatch_llm`.

**Known limitation.** The pipeline authenticates with `datawatch.token`, which
is imap-mcp's datawatch *service* token. datawatch accepts that token only for
reading imap-mcp's own secrets (`GET /api/external/secrets/{name}`). The LLM
proxy needs a **federation-peer token** with the `sessions:input`
capability. imap-mcp has no config field for a second token yet. That is a
backlog item, and it also needs an operator decision on the datawatch side,
because `sessions:input` grants more than LLM access.

Until that lands, datawatch rejects the call with 401 or 403. The pipeline
treats this as a permanent classifier failure. It logs
`classification failed, using defaults`, stores the embedding and marks the
message `done` **without tags**. Leave `classify.provider` set to `ollama`.

## Two lanes

Each queued message is in one of two lanes:

| Lane | What goes in it |
|------|-----------------|
| **new** | Messages whose UID is higher than anything already cached in a folder that had been synced before. This is mail that arrived since the last cycle. |
| **backfill** | Everything else: the first sync of a folder, a UIDVALIDITY rebuild, window growth, and folders re-fetched after a `cache_sweep` |

Each batch fills up from the **new** lane first. Backfill gets only the room
that is left. Within a batch, every new-lane message finishes before any
backfill message starts.

**The new lane is never rate-limited, windowed or gated.** Only backfill is
throttled, so fresh mail stays searchable within one sync interval plus a few
seconds, even during a large backfill.

## Throttles

| Throttle | Applies to | Setting | Default |
|----------|-----------|---------|---------|
| Concurrency cap | Both lanes. A separate cap for embed calls and for classify calls. | `concurrency` | 2 |
| Rate limit | Backfill only. A token bucket of N messages per minute (burst N). | `backfill_per_minute` | 30 (0 = unlimited) |
| Gates | Backfill only | see below | |
| Backoff | Both lanes | `backoff_max_seconds` | 300 |

Semantic-search queries share the embed concurrency cap.

### Gates

Before a batch takes any backfill, the pipeline checks each gate. The first
gate that refuses pauses backfill for that batch. The reason shows as
`backfill_paused` in `enrichment_status` and `/api/health`, and the log
records each pause and resume.

| Gate (`backfill_paused` prefix) | Active when | Pauses backfill when |
|---------------------------------|-------------|----------------------|
| `backfill_window` | `backfill_window` is set, for example `"22:00-07:00"` | The local time is outside the window. The window may wrap midnight; the start is inclusive and the end exclusive. |
| `ollama_load` | `yield.enabled` (default true) | Models **other than** our embed and classify models are loaded on the embed Ollama (`GET /api/ps`) and take more than `yield.max_foreign_resident_gb` (default 8). This is the "someone else is using the GPU" check. |
| `datawatch_capacity` | `yield.enabled`, a `datawatch` block, and at least one pool in `yield.datawatch_pools` (or the datawatch classify provider, which adds `llm:<datawatch_llm>`) | A watched datawatch capacity pool is full (`held + external >= limit`), or a waiter is queued on it (`GET /api/capacity`) |

The `ollama_load` and `datawatch_capacity` results are cached for 30
seconds.

**Gates fail open.** If a gate's source is unreachable, or returns something
other than 200 with readable JSON, the gate allows backfill. Backfill then
runs at the concurrency cap and the rate limit.

**Known limitation for `datawatch_capacity`.** `/api/capacity` needs a
federation-peer token with `autonomous:read`. imap-mcp sends its service
token, which datawatch refuses. The gate therefore always fails open. It never
pauses backfill, and it adds a request every 30 seconds. Leave
`yield.datawatch_pools` empty until imap-mcp supports a separate peer token.
The `ollama_load` gate does not depend on datawatch.

### Backoff

A **transient** provider error is a network error, an HTTP 429 or a 5xx. Each
one extends a global backoff of `5 s × 2^(consecutive failures − 1)`, capped
at `backoff_max_seconds`. No batch runs in either lane until the backoff ends.
One successful message resets the failure count.

Non-transient errors, such as a 404 for an unknown model, do not back off. A
non-transient **classify** error does not fail the message; it is stored
without tags. A transient classify error is retried.

### Attempts and the error state

Each try adds one to the message's attempt count. A failed try puts the
message back in the queue. When the count reaches `max_attempts` (default 3),
the message is marked `error`, its `last_error` is kept, and
`enrichment.error` is published with `{message_id, error}`. Messages in
`error` are not retried automatically.

To retry them, re-fetch them:

```
cache_sweep { errors_only: true, dry_run: false }
```

The sweep deletes those cached copies and resets their folders' sync state.
The next sync cycle fetches them again and queues them fresh in the backfill
lane.

### Restarts

At startup the pipeline puts any message left `processing` by a crash or a
stop back to `pending`. Attempts already counted stay counted.

## Status and control

### `enrichment_status` / `GET /api/enrichment/status` (`read`)

```json
{
  "enabled": true,
  "embed_provider": "ollama:nomic-embed-text",
  "classify_provider": "ollama:qwen3:1.7b",
  "pending": {"new": 0, "backfill": 1840},
  "processing": 2,
  "done": 5210,
  "errors": 4,
  "duplicates": 37,
  "done_last_hour": 1790,
  "oldest_pending_seconds": {"backfill": 7200},
  "backfill_paused": "backfill_window: outside backfill window 22:00-07:00",
  "backoff_seconds": 20,
  "consecutive_failures": 2,
  "last_error": "embed: ollama:nomic-embed-text: ..."
}
```

| Field | Meaning |
|-------|---------|
| `pending` | Queue depth by lane |
| `oldest_pending_seconds` | Lag in each lane: the age of the oldest pending item |
| `done_last_hour` | Throughput |
| `errors` | Messages that reached `max_attempts` |
| `duplicates` | Cached copies skipped by Message-ID deduplication |
| `backfill_paused` | Present while a gate pauses backfill |
| `backoff_seconds`, `consecutive_failures` | Present during backoff |
| `last_error` | The most recent failure since startup |

The MCP tool accepts an `account` argument but ignores it. The counts are
global. For per-account state counts, use
`GET /api/accounts/{account}/stats`.

`GET /api/health` repeats the main figures (`embed_provider`,
`classify_provider`, `pending`, `done_last_hour`, `errors`,
`oldest_pending_seconds`, `backfill_paused`, `backoff_seconds`) next to the
enrichment config. See [rest-api.md](rest-api.md#health).

### `trigger_enrichment` / `POST /api/enrichment/trigger` (`admin`)

```
trigger_enrichment { limit: 200 }
```

```bash
curl -sS -X POST http://127.0.0.1:8765/api/enrichment/trigger \
  -H "Authorization: Bearer <token>" -H "Content-Type: application/json" -d '{"limit": 200}'
```

A trigger runs one batch of up to `limit` messages (default 50) right away:
new lane first, then backfill. It **bypasses** the window, the gates and the
rate limit. It does **not** bypass backoff (a trigger during backoff does
nothing) or the concurrency caps. Only one trigger can be pending at a time.
REST then returns `"triggered": false`, and the tool says a run is already
queued. The tool's `account` argument is ignored.

Both return 503 or a tool error when enrichment is disabled.

## Config

```yaml
enrichment:
  enabled: true
  ollama_url: http://localhost:11434
  embed_model: nomic-embed-text
  llm_model: qwen3:1.7b
  batch_size: 20
  # embed:    { url: http://gpu-node.example.com:11434, model: nomic-embed-text }
  # classify: { provider: ollama, url: http://localhost:11434, model: qwen3:1.7b }
  concurrency: 2
  backfill_per_minute: 30
  max_attempts: 3
  backoff_max_seconds: 300
  backfill_window: ""          # e.g. "22:00-07:00"
  yield:
    enabled: true
    max_foreign_resident_gb: 8
    # datawatch_pools: []      # see the known limitation above
```

`auto_sync` is accepted but not used.

| Environment variable | Overrides |
|----------------------|-----------|
| `IMAP_MCP_OLLAMA_URL` | `ollama_url` |
| `IMAP_MCP_ENRICHMENT_CONCURRENCY` | `concurrency` |
| `IMAP_MCP_ENRICHMENT_BACKFILL_PER_MINUTE` | `backfill_per_minute` |
| `IMAP_MCP_ENRICHMENT_MAX_ATTEMPTS` | `max_attempts` |
| `IMAP_MCP_ENRICHMENT_BACKOFF_MAX_SECONDS` | `backoff_max_seconds` |
| `IMAP_MCP_ENRICHMENT_BACKFILL_WINDOW` | `backfill_window` |
| `IMAP_MCP_ENRICHMENT_CLASSIFY_PROVIDER` | `classify.provider` |
| `IMAP_MCP_ENRICHMENT_YIELD` | `yield.enabled` |

## Tuning

- **First sync of a big window.** Expect a large backfill queue. With the
  defaults, backfill runs at up to 30 messages per minute, about 1,800 per
  hour. Watch `pending.backfill` and `done_last_hour`.
- **Shared GPU.** Keep `yield.enabled` on. It pauses backfill while other
  models hold the GPU. Lower `max_foreign_resident_gb` if small foreign
  models still slow your other work.
- **Day-time contention.** Set `backfill_window` to your quiet hours, for
  example `"22:00-07:00"`. New mail is still enriched right away.
- **Dedicated GPU, no contention.** Raise `backfill_per_minute`, or set it to
  0 for no limit, and raise `concurrency` to what your Ollama serves in
  parallel. Raise `batch_size` too, because one batch every 10 seconds caps
  throughput at `batch_size × 6` per minute.
- **Flaky provider.** Raise `max_attempts` so short outages do not leave
  messages in `error`. `backoff_max_seconds` bounds how long the pipeline
  waits between probes.
- **Lag on new mail.** `oldest_pending_seconds.new` should stay small. If it
  grows, the provider is too slow or in backoff. Check `last_error` and
  `backoff_seconds`.
- **Split embed and classify.** Point `embed.url` and `classify.url` at
  different Ollama nodes to spread the load. Note that the `ollama_load` gate
  watches only the embed node.
- **Catch up now.** Run `trigger_enrichment` with a larger `limit` to process
  a backlog right away, outside the window and gates.
