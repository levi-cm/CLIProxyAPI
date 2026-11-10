# Claude 5-hour and Fable window routing (2026-11-10)

Branch `integration/claude-limits-20261110`, based on production `206fc86e`.
Deployed commit `ee4650b6`, image `cli-proxyapi-fork:claude-limits-ee4650b6`.

## Problem

On 2026-11-10 the focus account hit its 5-hour limit at 18:42, and the next
account at 20:00. Each event caused about three minutes of
`409 unsafe_affinity_unavailable` for running conversations, about 345 failed
requests in total. The unavailable-owner exception for Claude compared the route
namespace to `claude`, but the manager routes every request under `mixed`, so
the exception never ran in production. Its tests used `claude`.

## Commits

- `4484b167`: the bound credential decides whether a Claude turn may move. The
  selector tracks the 5-hour window (all models) and the Fable-scoped weekly
  window `7d_oi` (Fable only) per account. It learns them from every response,
  including 429 refusals, and from the usage poller's `limits` list (`session`,
  and `weekly_scoped` for "Fable"). A full window takes the account out of
  routing until its reset. If every account is full, upstream decides.
- `45866765`: turns referencing files, containers or server tool results keep
  their 409. Stale readings are ignored: an earlier window, or an open reading
  of a window already seen full.
- `60da129b`: reset-less readings no longer erase a known full window.
  `container: null` and fresh containers are portable.
- `91b141df`: custom-skill containers stay bound. Limit state is fenced by the
  registration epoch, and the poller hands its windows to the selector directly.
- `ee4650b6`: tests for custom skills and polled Fable window retention.

## Verification

- `go test ./...` (111 packages); `-race` on sdk/cliproxy/auth,
  internal/auth/claude, internal/accountpolicy and sdk/cliproxy; build; vet;
  `gofmt -l` clean; `git diff --check`; go.mod unchanged.
- Every fix has a test that fails without it, run against provider `mixed`.
  The real-manager tests cover a late stream result and a polled Fable window
  followed by an Opus response.
- Independent Codex (gpt-6.1-sol) review, four rounds:
  - Rounds 1–3 were BLOCKING: account-bound moves, stale completion order,
    reset-less readings, fresh and custom-skill containers. All were fixed.
  - Round 4 PASS.

## Deployment

Interrupt release `72e45fc959`, outage budget 90 s:
- Interruption: 7.173 s, 5 requests and 17 connections interrupted.
- Rehearsal: candidate start 24.0 s, rollback 4.0 s.
- Live checks passed, and the deploy's own Codex verification passed.
- Rollback container: `cli-proxy-api-rollback-72e45fc959`.

During the first three hours, 2662 Claude requests returned 200, with no 409
or 429.
