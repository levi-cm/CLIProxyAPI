# Codex ownership and capacity repair

Base: serving revision `b8a7383063021044f950da735597a0fc38ff5aa3`.
This candidate contains no upstream upgrade. Production configuration and state
are not inputs to its tests, and no candidate writer may open them.

## Ownership

The detector checks actual Responses continuation fields, the supported reasoning
and WebSocket create envelopes, and typed reasoning, compaction and reference
items. It ignores function schemas, tool choice, arbitrary tool-result data and
JSON text. Existing caller/provider/session/model ownership and durable identity
checks remain authoritative. An unrecoverable owner still returns 409: start a
new conversation using a plain-text handoff; do not copy opaque encrypted state.

Ownership rejections emit `account-policy-rejection` logs with the request trace,
classification, hashed caller/session identity, bounded canonical model, binding
lookup outcome and continuation category. Full request logging is unnecessary.

## Capacity recovery

`turn_rate_limit_exceeded` and the observed Codex-at-capacity message represent
transient admission pressure. They do not exhaust quota, disable credentials,
escalate quota cooldown, or request redemption. Genuine `usage_limit_reached`
continues through existing quota and mandatory refill handling.

Extended recovery is opt-in at `upstream.codex.capacity-retry`. The staged overlay
in `deploy/profiles/codex-capacity-repair.yaml` enables a 30-minute recovery budget,
8 attempts including the first, 16 pending requests globally, 8 per logical
account and 128 MiB of conservative pending-memory accounting. Admission also
requires at least 1 GiB of Linux MemAvailable and applicable memory-cgroup
headroom **after** the new reservation's charge. Measurement failures decline
waiting. No transport timeout is added; an established attempt can finish after
the recovery budget, but another attempt cannot begin after it expires.

A valid Retry-After delay or date is authoritative, including zero. A deadline
outside the remaining budget returns the original capacity error immediately.
Missing or invalid hints use 30-second exponential backoff, up to five minutes,
with positive jitter below the cap. Extended waiting disabled means returning a
recognized capacity rejection immediately. The model is never substituted.

Waiters share one probe per logical credential and canonical execution model.
Each new attempt revalidates account identity and eligibility. Canceled waiters
release reservations immediately; upstream attempt counters are released during
waiting. There is at most one admitted waiter per caller/session/model. New
requests require an explicit session to enter extended recovery. Capacity gates
are transient and bounded to 256 retained errors, each at most 64 KiB; they are
not written to quota or conversation state. Active reservations survive config
reloads; new admissions use the reloaded limits. Recovery is local-router only;
Home dispatch retains its authoritative existing lifecycle.

Only explicit admission failures before any generated item, tool activity or
unknown business event may retry. Streams already handed to the downstream
consumer never replay. SSE/bootstrap and WebSocket paths preserve retry hints.
The outer API stream bootstrap cannot add retries to a capacity rejection.

`account-policy.automatic-spreading-enabled: false` disables automatic spreading
and retains earliest eligible renewal priority, including mandatory expiring
credit safeguards. Omission preserves the existing conservative spreading gates.

The authenticated v8 account-policy dashboard includes `capacity_recovery` with
aggregate `pending` and `pending_bytes`. Capacity logs contain hashed account and
model keys and attempt numbers. No credential data is added to these diagnostics.

## Deployment gate

This candidate is not authorization for a restart. The current serving process
owns its listeners directly and closes connections on stop. Before replacing
production, separately demonstrate listener/connection handoff, draining,
conversation-state preservation, exactly one authoritative writer and rollback.
An idle period and passing tests do not establish zero downtime.

Once that gate is proven, apply the overlay to a protected config copy, retaining
mandatory enabled `auto_expiring` automation, `automation_required`, expiry guard
and supported reset credit type. Validate loopback, LAN and Tailscale health,
authenticated models, the new-chat schema reproducer, genuine encrypted ownership,
complete SSE and two WebSocket turns. Verify from the affected remote terminal
as well as the host. These checks have not been run on the live candidate while
the availability gate is unresolved.

## Implementation decisions

- Missing explicit sessions and Home-dispatched execution decline extended waiting
  instead of sharing unverified identity or competing with Home authority.
- The profile opts into recovery; recognized capacity returns immediately when it
  is disabled. Clients must retry in these compatibility paths.
- Recovery fixes one canonical execution model from an existing alias pool; it
  does not substitute another model to mask capacity.
- Actual upstream HTTP statuses are preserved. Capacity failures carried as
  terminal events inside HTTP 200 use the existing synthetic 429 contract.
- Unrecoverable historical ownership requires a plain-text handoff into a new
  chat; no code attempts to reconstruct or guess the missing owner.

## Candidate validation, 2026-10-26

The complete Go suite passed after the final review fixes (111 packages with
tests). Relevant race tests passed across auth/recovery, executor helpers,
Codex executors, base API handlers, OpenAI/WebSocket handlers, management,
real fixture API transports and configuration. The required server build passed
through `scripts/go-limited.sh` using Go 1.26.10. All Go files were formatted.

Permanent regressions cover the original reproductions, CLI 0.163.0/0.164.0
function schemas over fixture HTTP, real pinned WebSocket capacity delivery,
ownership/restart/isolation, recovery deadlines/attempts/cancellation/probes,
resource admission, config round trips/reload, scoped-error-rule bypasses,
terminal SSE hints, actual HTTP statuses and bounded retained errors.

A saturated queue retained 16 requests: 33,556,492 conservatively accounted bytes
and approximately 4.0 MiB additional measured heap with a simulated additional
3 GiB browser load. The native host measurement succeeded with approximately
5.3 GiB headroom after cgroup-v2 root handling was corrected. These are fixture
and host observations, not a live traffic capacity guarantee.

The final fresh review's seven important findings were fixed with reproducing
regressions; no findings were deferred. Production stayed on the original image,
with unchanged container start time and restart count. Its health and management
paths returned HTTP 200 on loopback, LAN and Tailscale. The candidate has not
replaced production; affected remote-terminal acceptance remains gated on the
separately demonstrated zero-downtime handoff.
