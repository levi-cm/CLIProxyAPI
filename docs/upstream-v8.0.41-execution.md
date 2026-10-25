# Upstream v8.0.41 integration execution

Source: user-approved upstream merge and production deployment plan, 2026-10-23.
Fork baseline: `2896a388b2d43006477a5e4cb3670cce73c544f5`.
Pinned upstream: `8289ec4c71ad914d0612c2776352b76ba3d355a6`.
Recovery ref: `recovery/pre-upstream-v8.0.41-20261023`.
Implementation branch: `integration/upstream-v8.0.41`.

## Constraints

Preserve the original dirty checkout, credentials, runtime state and serving router.
Use isolated fixture state only. Keep automatic reset redemption mandatory, its
600-second expiry guard, supported credit type, identity checks and one writer.
Preserve ownership, routing overrides/splits, usage history, catalogs, SDK/plugin
interfaces, final payload rule precedence and allowed network deadline boundaries.
Keep the installed gateway plugin disabled. Go 1.26.10 or newer is required.

The latest deployment plan authorizes one maintenance window of at most 60 seconds
including rollback, with a target under 15 seconds. Admission gating must preserve
established connections; abandon the window if idle cannot be confirmed in five
seconds. Require startup, authenticated API and fresh inference by 25 seconds.
Do not cut over unless isolated upgrade and rollback rehearsals, forced candidate
failure, state compatibility and writer exclusion demonstrate sufficient margin.
Never overwrite newer ownership, usage or reset outcomes with old backups.

### Task 1: Baseline and complete merge

Verify baseline, create recovery ref and isolated worktree, run baseline tests.
Merge the complete pinned upstream with --no-ff --no-commit, resolve each conflict
individually, and record decisions. Preserve upstream security/affinity/fencing.

### Task 2: Compatibility and regression validation

Audit routing/ownership, credentials/quotas, streams/WebSockets, usage/history,
APIs/SDK/plugins, every changed executor payload path and network deadline.
Add focused regressions for discovered integration failures; fix stale docs.
Run all Go tests, affected race suites, JavaScript/client/companion suites,
fixture browser checks, gofmt, diff checks, server build and container smoke.

### Task 3: Deployment rehearsal and source advancement

Rehearse upgrade and forced-failure rollback with realistic temporary state,
connection draining/admission, one writer and measured interruption. Review the
complete branch with a fresh reviewer. Advance main only when all required source
gates pass and its starting tip remains unchanged, preserving unrelated files.

### Task 4: Production deployment and acceptance

Only after rehearsal gates pass, preserve image/spec/config/environment/catalogs/
credentials/history in a protected backup, verify restoration, and deploy using
the rehearsed maintenance procedure. Verify actual serving build, auth, fresh
inference, SSE, WebSocket turns/reconnect, pre-cutover encrypted continuation,
ownership failure closure, policy/history, mandatory refill settings and all
three interfaces. Remote clients must validate remote reachability when available.
Monitor for 30 minutes and report all unavailable/failed/skipped checks.

## Evidence

Detailed commands, decisions and outputs are in the isolated worktree under
`.superpowers/sdd/upstream-v8.0.41/`. No deployment or completion is implied by
this execution record; consult the recorded validation results and blockers.

Validation results and deployment blocker: [validation report](upstream-v8.0.41-validation.md).
