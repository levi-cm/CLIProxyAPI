# Local worktree consolidation (2026-10-25)

The user authorized verification and local source merging of all outstanding
worktrees. This supersedes the earlier source-only hold on advancing `main`;
it does not authorize a production deployment or waive availability requirements.

Integration branch: `integration/local-worktrees-20261025`.
Original local main: `2896a388`.

## Worktree inventory and reconciliation

Fifteen worktrees existed besides the main checkout. All were inspected for
branch ancestry, uncommitted changes and genuinely unrepresented patches.

| Existing worktree group | Source | Integration |
| --- | --- | --- |
| Renewal routing | `74ae4b97` | Base of the consolidation; includes CPU limits and verification corrections. |
| Upstream candidate | `bae50bcb` | Normally merged as `887b2dab`; pinned upstream v8.0.41, renewed routing retained. |
| Detached upstream review | `37a7e52b` | Already an ancestor of `bae50bcb`, including its subsequent fixes. |
| adapter, companion, core, lifecycle, operators, routing, transport, ui, usage | Nine historical feature tips | All patches already represented in both canonical `feat/account-policy` and original main. Recorded branch ancestry with merge `df87fae5`, without replaying obsolete source. |
| baseline-check | `8ef43e4d` | Already an ancestor of original main. |
| allowance-history | `2896a388` | Already identical to original main. |
| catalog-hotfix | Original `17199e47`, new `86b47949` | Existing committed work already integrated; four untracked helper source/docs/test files were verified, committed in their original worktree and normally merged as `d99e6862`. |

Stable patch IDs and `git cherry` independently established that all 64
ancestry-unique commit entries across the nine historical feature branches were
already represented. The ancestry-only merge's before/after tree was identical:
`cfea294c43feabd4c8afcaa74ce71988a8e55e5c`.

The catalog import contains only `.gitignore`, `README.md`, `refresh_catalog.py`
and `test_refresh_catalog.py` under `ops/catalog-compat/`. Original ignored
staged catalogs, lock files and caches remain in their original worktree.
The helper requires explicit invocation and output/template arguments; it is not
installed, scheduled or activated by this merge.

Every existing worktree HEAD is an ancestor of the resulting source integration.
All original worktrees are retained; no worktree or user file was deleted.

## Combined review

Two independent agents verified patch equivalence and the combined source.
Renewal policy/pressure/forecast and CPU wrapper production files match the
previously verified routing candidate. Payload executors, response interrupt
identity, usage/ledger, plugin ABI/API and stream lifecycle retain the tested
upstream candidate's implementations. The release workflow retains Go 1.26.10
and all five CPU-limited build invocations. Plugin ABI remains 1 and RPC schema 6.

## Production separation

Before advancing the source checkout, the actual production mounts were
inspected: config.yaml, auths, logs, plugins, static and account-policy-state.
No changed tracked file lies under those mounts; auths/.gitkeep is unchanged.
The running binary is inside the existing image, not mounted source code.
Original modified `.dockerignore` and five untracked files must remain intact.
The config file inode must remain unchanged.

Production remains on `cliproxyapi-fork:allowance-2896a388`; this task performs
no stop, restart, recreation, deployment, configuration change, credential write,
second policy writer or synthetic production inference.

The earlier realistic-volume recovery rehearsal failed its readiness/rollback
budget. Verified connection draining, one-writer ownership, durable state
compatibility, rollback and zero-downtime handoff remain required before any
production upgrade. An approved quiet window alone does not satisfy these gates.
See [the original readiness evidence](upstream-v8.0.41-validation.md).

## Fresh validation on combined source

All Go compilation used the half-CPU wrapper and Go 1.26.10.

- Full uncached `./scripts/go-limited.sh test ./... -count=1`: PASS
  (111 tested packages), `/tmp/cliproxy-consolidation-full.log`.
- Broad affected race suite: PASS (17 package groups),
  `/tmp/cliproxy-consolidation-race.log`. Includes policy/ownership/state,
  APIs/management, executors/helpers, SDK/auth/usage, interrupts, stores and WS.
- Required server build and fixture preview build: PASS; temporary server binary
  removed. Focused vet and diff checks: PASS.
- JavaScript helpers: 38/38 PASS, `/tmp/cliproxy-consolidation-js.log`.
- CPU limiter regressions: 6/6 PASS. Catalog helper regressions: 5/5 PASS.
- Companion suite: 10/10 PASS, including the previously skipped real Caddy
  private-edge fixture smoke. A cached Caddy 2.11.7 binary was located and used
  only for the temporary port 18417 fixture with an offline provider. HTTP/SSE,
  management-source gates and WebSocket reconnect checks passed; the temporary
  listener was stopped. Host-side self-connections do not certify a separate
  remote device or production deployment. Final log:
  `/tmp/cliproxy-consolidation-companion-complete.log`.
- All five fixture-only browser suites: PASS (history/manual routing, dashboard,
  observations, weekly timeline and mandatory automation controls).
- Eight light/dark accessibility snapshots: 100%, with zero reported failures.
  Logs: `/tmp/cliproxy-consolidation-browser-*.log`.
- Two independent combined-source reviews: no additional blocker.

The source-only main fast-forward is permitted only to this tested commit tree,
with a recovery ref and verification that preexisting local files and the live
config inode remain unchanged. All original worktrees remain available.
