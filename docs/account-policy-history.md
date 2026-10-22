# Permanent usage, allowance history, and temporary routing

This candidate adds a permanent archive and dashboard controls. It has **not** been deployed. Production must continue serving from the existing container until an independently verified zero-downtime handoff is available.

## Enable collection

The default is off. Merge this setting into the candidate's existing configuration:

```yaml
account-policy:
  usage-history-enabled: true
```

This is independent of `enabled`, `observations-enabled`, usage statistics, and reset automation. It records provider attempts even when smart routing and provider observation collection are disabled. Enabling history alone does not poll providers or redeem credits. Existing accepted provider observations are archived when history is enabled. The dashboard includes an independent switch. Startup configuration determines initial collection; management changes persist in the policy journal.

## Authentication and scope

External applications use the same inference API key as `/v1/account-policy/usage`:

```http
Authorization: Bearer <inference-api-key>
```

Every accepted inference key can read the global operational view and change the shared routing override. These routes require an authenticated principal even when anonymous inference is permitted. They require no session ID. The existing session-scoped usage and read-only MCP contracts remain unchanged. Equivalent routes under `/v8/management/account-policy` retain management authentication and remote-access restrictions. Responses use `Cache-Control: no-store`; timestamps are UTC. CORS explicitly supports Authorization and JSON with PUT/DELETE preflight. Store keys outside URLs and page persistence.

| Method | `/v1/account-policy` path | Contract |
| --- | --- | --- |
| GET | `/accounts` | Sanitized choices, eligibility, current quota and capabilities |
| GET | `/routing-override` | Shared state and revision |
| PUT | `/routing-override` | Create or replace using expected revision |
| DELETE | `/routing-override?expected_revision=12` | Cancel using expected revision |
| GET | `/allowance-history` | One credential, `window=weekly` or `5h`, explicit `from` and `to`, at most 31 days |
| GET | `/usage-history/requests` | Stable, paginated provider attempts |
| GET | `/usage-history/summary` | UTC day/month/year groups by provider, execution model and credential |
| GET | `/usage-history/export` | Stream `format=csv` or `ndjson` |
| GET | `/usage-history/status` | Collection, storage health, write errors and uncertain coverage |

Management `/accounts` preserves its existing dashboard-compatible response. Other new handlers are shared across both mounts.

### Override example

```http
PUT /v1/account-policy/routing-override
Content-Type: application/json
Authorization: Bearer <inference-api-key>

{"credential_id":"selected-credential","duration_hours":6,"expected_revision":12}
```

```json
{"schema_version":1,"id":"override-uuid","revision":13,"credential_id":"selected-credential","provider":"codex","started_at":"2026-10-22T10:00:00Z","expires_at":"2026-10-22T16:00:00Z","state":"active"}
```

Durations are integers 1–24. Replacing starts a new duration. Mutations return authoritative state; stale revisions return HTTP 409. Errors retain `{"error":{"code":"conflict","message":"..."}}`. After an ambiguous network result, reread state before retrying. Success atomically clears legacy force-account. Conflicting permanent force changes are rejected while an override is active.

The preference applies to eligible new work for that provider across all callers, ahead of deadline and allowance-reserve preferences. Holds, authentication, model support and actual quota/cooldown restrictions still apply. Unsupported models use ordinary eligible routing. Active streams are not interrupted. Existing account-bound and encrypted conversations keep their owner; replayable conversations use the existing proven idle-boundary rules. The override never redeems credits. Absolute expiry is checked during selection, and maintenance persists terminal expiry, removal, reassignment, policy disablement and exhaustion. A verified ordinary short or weekly window signal may terminate immediately; uncertain throttling only requests discovery. Terminated overrides never reactivate on refill.

### History queries

```http
GET /v1/account-policy/usage-history/requests?from=2025-01-01T00%3A00%3A00Z&to=2027-01-01T00%3A00%3A00Z&provider=codex&limit=100
GET /v1/account-policy/usage-history/summary?from=2025-01-01T00%3A00%3A00Z&to=2027-01-01T00%3A00%3A00Z&group=year
GET /v1/account-policy/allowance-history?credential_id=selected-credential&window=weekly&from=2026-10-01T00%3A00%3A00Z&to=2026-10-08T00%3A00%3A00Z
```

Time filters use an inclusive `from` and exclusive `to`. Request, summary and export filters support `provider`, `model` and `credential_id`. Model filters match requested, execution or response model identifiers. Request pages default to 100 and cap at 500. Follow `next_cursor` with unchanged filters; it anchors the end of the archive so concurrent appends do not shift the sequence. An empty terminal page is possible. Summary `group` is `day`, `month` or `year`. Large summaries stream one calendar period at a time. NDJSON retains the complete allowlisted schema; CSV includes measurement flags and all model identifiers. Exports contain provider attempts, not unique client requests or billing totals. A terminated/truncated export or JSON stream is incomplete and must be retried.

### Measurement semantics

Request records preserve execution UUID, parent execution UUID for additional models, available parent trace, provider/credential references, three model identifiers, UTC start, duration, streaming/generation flags, outcome, reported failure status, authentication category, service tiers, canonical token accounting and explicit presence flags. They never contain prompts, completions, keys, tokens, authorization headers, or raw upstream errors. The observed HTTP response status is retained when available; an unavailable status stays zero rather than inventing an HTTP response code. Missing model identifiers stay unknown. Additional-model records keep their own execution model.

`tokens` uses the SDK v2 mutually exclusive input and output breakdown. Cache reads, cache writes and reasoning are not added twice. Provider-reported cache-write five-minute/one-hour splits are retained. `measurements` records what the upstream actually reported; reported zero differs from absence. `measurement_quality` distinguishes complete, incomplete, inconsistent and unknown measurements; canonical `tokens.quality` describes arithmetic classification. Older plugin producers without presence metadata remain conservatively unavailable for cache percentages.

`cached_input_percent` is `100 * cache-read input / total canonical input`. Cache writes are excluded. A zero denominator, incomplete classification, or missing required measurements returns `null`. Summaries divide summed measured cache reads by summed measured input, and expose `cache_measured_attempts`, `cache_measured_input_tokens`, and `cache_measured_read_tokens` alongside total attempts. Thus 800 cached input tokens across 10,000 measured input tokens is 8%, even when per-attempt ratios differ.

Quota observations retain actual observation timestamps, quota scope/model, duration, used/remaining percent, permission state and expected provider reset. Internal continuity references prevent reassigned credentials from joining unrelated histories; a change marker breaks the trace. Events distinguish `manual_reset` confirmed in the operation journal, `natural_refill` confirmed across an observed reset boundary, and unexplained `allowance_change`. Future reset timestamps are expectations; the UI never projects an observed allowance into the future. The daily graph uses the selected display time zone and actual 23/25-hour Berlin days.

## Archive compatibility and recovery

State lives under `<account-policy state-dir>/history`. Directories are 0700; files are 0600. Symlink paths are rejected, access is confined with `os.Root`, and an exclusive archive writer lock prevents competing writers. Requests and quota events have separate numbered, versioned append-only JSONL segments. Rotation at 16 MiB creates another file and never deletes old history.

Raw records are authoritative. Segment indexes contain bounds, sparse offsets, execution IDs and daily provider/model/credential totals. A derived disk radix index uses at most 256 entries per leaf for UUID deduplication, so ordinary appends do not scan historical segment indexes. Missing or size-mismatched indexes are rebuilt one segment at a time; queries load bounded pages and indexes rather than years of raw records. A corrupt complete line or incomplete final write is retained byte-for-byte, flagged unhealthy, and subsequent writes use another segment. No complete record is silently truncated. Accepted raw appends are synced before the writer reports success. Orderly SDK shutdown drains the usage dispatcher before closing archive storage.

Adoption imports surviving `usage.jsonl`, `usage.1.jsonl`, and `usage.2.jsonl`, preserving originals. Content checkpoints skip completed imports; UUID deduplication makes interrupted imports restartable. First enabled initialization also runs adoption when collection is enabled after startup. Surviving confirmed reset operations are imported from the operation journal. Legacy records keep their surviving counts and model alias, while missing execution/response model, presence, stream, generation and pricing context remain unknown. Earlier deleted history cannot be reconstructed.

Storage failures never stop inference or trigger retention deletion. Status exposes unhealthy segments, write errors and uncertain periods. Coverage cannot be complete during failures, collection-disabled periods, abrupt process loss before dispatch, missing provider measurements, or before adoption. Keep status alongside exported data. Disk capacity is an operational responsibility because retention is indefinite.

## Delivery and rollback gate

This branch is a tested candidate, not a deployment. The single directly exposed production container has no verified zero-downtime replacement path. Do not restart, recreate, replace, or deploy it routinely.

Before upgrading, rehearse continuous loopback/LAN/Tailscale inference, new request arrivals while existing HTTP/SSE and WebSocket connections drain, one authoritative writer for credentials/policy/overrides/reset journals/conversation ownership, durable owners established during the transition, and rollback preserving both those records and model availability. Target zero interruption; never exceed one second. Use only isolated fixture state during the rehearsal. If this cannot be demonstrated, leave production running.

For rollback, stop admitting work to the candidate, drain its active requests, preserve the entire policy directory and archive, then transfer sole writing authority using the rehearsed handoff. Older binaries ignore the new archive but must never overwrite newer policy state concurrently. Preserve a verified state snapshot and forward-written ownership/journal records; reverting only a container image is insufficient. Never remove the archive to roll back.
