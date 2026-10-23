# Permanent automatic reset redemption

User requirement: automatic refill redemption must never be switched off after
two resets appeared to expire unused. Keep the production proxy available and
preserve streams, WebSockets, conversation ownership and single-writer state.

Implementation base: 27267423, feature/allowance-history, isolated worktree
/home/levi/worktrees/cliproxy-allowance-history. No production code deployment.

Behavior:
- Selecting either automatic mode irreversibly latches automation_required in
  the existing policy journal. Existing automatic journals migrate at startup.
- Settings/API changes cannot clear the latch, disable the policy, choose off or
  notify, make the authoritative writer read-only, remove supported reset credit
  types or reduce the expiry guard below 600 seconds. Failure is HTTP 409 with
  error.code automation_required; rejected changes preserve live and disk state.
- Stale startup config cannot disable an established requirement. Reload errors
  retain the active settings. Explicit read-only replicas and owner.lock continue
  to prevent a competing reset writer; account/provider eligibility stays intact.
- UI disables those controls and imposes the lock even on already dirty drafts;
  other form choices stay editable. Enabling automation explains permanence.
- A scheduler that owns owner.lock persists the startup latch before dispatch,
  even with zero accounts. Constructors do not write shared state. Failed latch
  persistence retries on later scheduler ticks/wakes, with a diagnostic log.
- Updated agent guidance and rollback documentation prohibit disabling automation
  for maintenance. Activation of enforcement still needs the candidate binary.

Evidence:
- /tmp/cliproxy-automation-lock-red.log: old service accepted disabling requests,
  old API returned 200, and off startup/reload disabled automation.
- /tmp/cliproxy-automation-lock-browser-red.log: old UI kept disabling controls
  enabled despite a newly returned mandatory requirement and a dirty form.
- /tmp/cliproxy-automation-lock-focused.log: targeted policy/API/reload checks pass.
- /tmp/cliproxy-automation-lock-startup-red.log and -startup-green.log: independent
  review found a no-account persistence gap; reproductions failed, then passed
  after saving the latch under authoritative scheduler ownership.
- Independent reviewer accepted the fix and reran startup, legacy migration and
  second-owner regressions without Critical or Important findings.
- /tmp/cliproxy-automation-lock-browser.log: desktop/mobile mandatory controls,
  stale drafts, minimum guard, local forged-submit rejection, no provider writes.
- /tmp/cliproxy-automation-lock-js.log: all 37 JavaScript tests passed.
- Two legacy tests were updated for the intentional policy change: a genuinely
  observation-only installation uses off, while rollback cannot disable a policy
  that previously selected automatic redemption.

Final verification passed:
- Full `GOMAXPROCS=2 go test -p 2 ./...`:
  /tmp/cliproxy-automation-lock-full-final.log.
- Affected `go test -race -p 1` for internal/accountpolicy,
  internal/api/handlers/management, internal/api and sdk/cliproxy:
  /tmp/cliproxy-automation-lock-race.log, no race reports.
- Existing 100-account dashboard and history/single/split desktop/mobile suites:
  /tmp/cliproxy-automation-lock-dashboard-browser.log and
  /tmp/cliproxy-automation-lock-history-browser.log.
- Required server build succeeded; test-output removed. Changed Go files pass
  gofmt, JavaScript syntax checks pass, and staged diff passes whitespace checks.
- Fixture previews on loopback ports 18342 and 18343 were stopped by exact
  executable/argument match after browser verification.

Production: management diagnostics verified enabled=true, auto_expiring,
expiry_guard_seconds=600 and read_only=false. Serving container remained
95130a119c65ef0dc6137cdb06d4182c8e5affb43be223d67a84d065d976c95c, started
2026-10-05T19:45:21.269634547Z, restart count 0. The current binary lacks the new
enforcement. Do not claim it is active, add a second policy writer, or restart the
proxy to activate it. A verified zero-downtime handoff is still required.
Final live checks again confirmed the same configuration and container identity,
and HTTP 200 health on loopback, LAN and Tailscale. No production files or policy
state were modified during this implementation.
