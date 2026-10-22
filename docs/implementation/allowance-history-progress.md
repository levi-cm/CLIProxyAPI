# Remaining allowance, temporary routing, and permanent usage

Source: user implementation plan supplied 2026-10-22. Base 17199e47.
Candidate: /home/levi/worktrees/cliproxy-allowance-history, feature/allowance-history.
Production must remain untouched. No deployment is authorized without a verified uninterrupted handoff, draining, authoritative state ownership, and rollback rehearsal.

Tasks (sequential):
1. Permanent append-only usage archive, 16 MiB segments, recoverable indexes, indefinite retention, independent default-off setting, dispatcher drain.
2. Allowlisted canonical measurements, model identities, presence metadata, weighted cache percentage and coverage.
3. Every validated quota observation, honest reset markers, identity continuity, restartable legacy import.
4. Durable revisioned 1-24 hour shared override, hard eligibility and conversation ownership preserved, absolute expiry and terminal exhaustion.
5. Shared inference-authenticated and management APIs, stable pagination, streaming export, documentation.
6. Daily allowance SVG rows, real timestamps, gaps, DST, values table, filtered accounts.
7. Temporary routing controls and historical requests, summaries and exports, health display.

Pre-flight interfaces: ledger consumes SDK Record and canonical TokenBreakdown; service observation hook must run outside main lock; routing consumes optional service override preference and keeps current affinity safety; both auth surfaces use shared handlers; UI consumes common versioned APIs.
Ruling: use an external worktree to avoid changing production .gitignore. No production env or state copied.
Verification: fixture-only package tests, race tests, JavaScript and browser regressions, full Go suite, build then remove binary, gofmt, diff check.

Implementation checkpoint:
- Archive, canonical measurement flags, quota callback, global override state machine, shared APIs and dashboard surfaces implemented.
- Focused policy/routing/API/SDK/JS tests passing at intermediate checkpoints; full final verification still pending.
- Ruling: use a policy observation callback rather than importing the archive into accountpolicy. The SDK usage package imports logging/config, which would form an import cycle. Callback preserves independent archive ownership and outside-lock writes.
- Ruling: preserve incomplete tails in sealed unhealthy segments rather than truncating them; recovery appends to a new segment.
- Ruling: use existing scoped dashboard identity and visual system; no redesign.
- CORS preflight regression proved Authorization must be explicit rather than relying on wildcard alone.
- Added parser regression for partial final frames and confirmed-reset operation notification after journal persistence.

## Final review and rulings

One fresh whole-candidate reviewer identified eight Important findings. All were addressed:
1. Replaced historical UUID scans with a derived disk radix tree, bounded to 256 entries per leaf; ordinary append uses the active segment only.
2. Snapshot immutable quota read positions and release the archive writer mutex before streaming to clients.
3. Invalidate/reconcile cached offsets after uncertain writes and quarantine the affected segment before accepting another append.
4. Persist running markers and nonzero process start coverage bounds for abrupt-loss detection.
5. Preserve reported totals, reasoning-only updates, duration splits, explicit zeros, and zero-with-tier updates across streams. Keep existing SDK Model/Alias compatibility and add ExecutionModel for archive identity.
6. Use host identity existence/permanent-failure evidence separately from temporary eligible-account lists during authentication refresh.
7. Reject stale override revisions and use independent summary generations and reconnect guards.
8. Run restartable legacy adoption on first enabled initialization, including enablement after startup; seed confirmed reset events from the operation journal.

Ruling: the existing Gemini test deliberately discarded zero usage frames. The new presence contract supersedes that behavior; it now verifies that reported zero and final nonzero observations both retain their measurements, while final counters still win.
Ruling: retain the branch and isolated worktree as the requested candidate. No merge, push, or production deployment is part of delivery. The production availability gate remains unsatisfied.

## Verification

- Clean baseline: `go test ./...` passed from an archive of base 17199e47 in `/tmp/cliproxy-allowance-pristine`; log `/tmp/cliproxy-pristine-only.log`.
- Final complete suite: `go test ./...` passed; `/tmp/cliproxy-full-delivery.log`.
- Affected race suites: `go test -race -p 1 ./internal/accountpolicy ./internal/accountpolicyledger ./internal/accountpolicyapi ./internal/accountpolicyusage ./internal/runtime/executor/helps ./sdk/cliproxy/usage ./sdk/cliproxy/auth ./sdk/cliproxy ./internal/api` passed; `/tmp/cliproxy-race-serial.log`. Newly added real historical API/CSV/NDJSON/privacy contract also passed under race; `/tmp/cliproxy-api-contract-race.log`.
- JavaScript: `node --test internal/accountpolicyui/*_test.cjs` passed, 35 tests; `/tmp/cliproxy-js-verified.log`.
- Existing fixture browser regressions passed: 100-account pool, concurrent serving, graphs, drafts, keyboard focus, stale/failure recovery, dark/mobile rendering, runtime-only providers, memory-only key, and no provider writes; `/tmp/cliproxy-browser-dashboard-final.log`.
- New fixture browser regressions passed: desktop/mobile, seven daily rows, all 24 durations, shared apply/cancel, preserved selections, requested/executed/response models, cache percentage, and no document overflow; `/tmp/cliproxy-browser-reviewed.log`.
- Final visual inspection corrected the mobile filter layout to full usable fields; existing colours and typography retained. The mechanical design detector reported two pre-existing accent-border warnings in incumbent CSS; they were left unchanged.
- `go build -o test-output ./cmd/server` passed; verification binary removed. Go formatting and `git diff --check` passed.
- Earlier concurrent broad runs exposed intermittent WebSocket fixture closure and auth synchronization timing failures; the final full suite and serialized affected race run passed. These were not treated as evidence of production availability or a safe deployment.
- Read-only production verification: checkout HEAD remains 17199e47 and its original dirty/untracked file list is unchanged. No production process, container, listener, configuration, credential, journal or conversation binding was modified.

Task 1: complete - permanent archive, recovery, indefinite retention, durable indexes, independent collection and shutdown drain.
Task 2: complete - allowlisted measurements, three model identifiers, partial/zero presence, canonical accounting, cache coverage and observed HTTP status.
Task 3: complete - quota observations, identity continuity, honest refill/reset events, surviving usage/reset adoption.
Task 4: complete - durable revisioned temporary preference, 1-24 hours, cancellation/replacement, expiry/exhaustion/removal, hard eligibility and continuation ownership.
Task 5: complete - shared inference and management APIs, principal checks, CORS, stable pages, streaming summaries/exports and integration documentation.
Task 6: complete - daily remaining allowance SVG rows, account/window/period controls, real timestamp and DST behavior, explicit coverage states and values table.
Task 7: complete - shared override controls, countdown without requests, paginated requests, summaries, exports and archive health.
Production deployment: blocked - no verified uninterrupted handoff, draining, sole state writer and rollback rehearsal.
