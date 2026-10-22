# Percentage routing extension

Base: 65992243 on feature/allowance-history in the existing isolated worktree.
Production must remain untouched: no restart, settings mutation, deployment, or
second writer against production credentials/policy state.

User request: future 50/50 mode configurable from 1/99 to 99/1. Ruling: interpret
99/100 as 99/1 so percentages total 100. Extend the existing temporary override,
retaining 1-24 hour duration, revision checks, cancellation, expiry and fallback.

Implementation decisions:
- Add optional mode=split and accounts=[{credential_id,percent},...] to the existing
  PUT routing-override API. Exactly two distinct Codex accounts, integer percentages
  1-99, sum 100. Existing single-account request bodies remain compatible.
- Save both logical identities and percentages in the policy journal; no account
  ownership IDs are exposed. Exhaustion/removal/reassignment of either participant
  ends the override and normal smart routing chooses an eligible survivor.
- Use smooth weighted round robin for eligible new and proven safe idle replayable
  decisions, ahead of deadline/reserve preferences. Count decisions globally per
  override, not tokens. Preserve strict/parent/encrypted/active owner protections.
- Reuse the existing weighted picker on lightweight local weight descriptors;
  never mutate registered credentials or their static weights. Reset credits on
  override replacement; unavailable/model-incompatible members do not accumulate
  catch-up credit, and unrelated eligible accounts remain normal fallbacks.
- Add Single account / Percentage split UI, second account, default 50% first
  account numeric input (1-99), complementary output and active split status.
- Keep existing management and inference-key authentication, including CORS.

Steps:
1. Add failing policy lifecycle, ratio/continuity/concurrency, API and UI tests.
2. Implement durable split state and participant validation/termination.
3. Integrate weighted selection within the existing ownership safety boundaries.
4. Add API contract, dashboard controls, documentation and browser coverage.
5. Run focused tests, one independent review and necessary regression fixes.
6. Run full Go suite, affected race suites, JS/browser checks and server build.
7. Commit the worktree changes, verify production identity unchanged, retain branch.

Status: implemented in the isolated worktree. Tests use temporary state and fixture
accounts. Production has not been changed or deployed.

Implemented:
- Durable split mode, participant identities, strict API validation and backwards
  compatible single-account replacement, expiry and revisioned cancellation.
- Shared concurrency-safe percentage selection, all 99 complementary settings,
  reserve/deadline bypass within existing eligibility and conversation safeguards.
- Standalone controls, active percentages, responsive layout, integration contract.

Validation:
- TDD: policy/selector tests first failed before implementation; selector assertions
  demonstrated all requests going to one account. Split API and UI tests also
  failed against the original single-account contract before the implementation.
  Logs: /tmp/cliproxy-split-red.log,
  /tmp/cliproxy-split-model-green-selector-red.log,
  /tmp/cliproxy-split-api-red.log and /tmp/cliproxy-split-ui-red.log.
- Focused affected Go packages passed: /tmp/cliproxy-split-focused.log.
- Full `GOMAXPROCS=2 go test -p 2 ./...` passed:
  /tmp/cliproxy-split-full.log.
- All 37 JavaScript tests passed: /tmp/cliproxy-split-js.log.
- Desktop 1440px and mobile 390px browser suites passed, including a 100-account
  pool, 24 durations, single/split apply/cancel, 50/50 and 1/99 and 99/1,
  retained drafts, exact active percentages and no overflow:
  /tmp/cliproxy-split-dashboard-browser.log and /tmp/cliproxy-split-browser.log.
  Expanded options initially exposed intrinsic select-width overflow; a dedicated
  wrapping grid fixed it. The history browser script now explicitly creates a
  100-account fixture, guarded by fixture_only evidence, to retain this coverage.
  Screenshots: /tmp/cliproxy-percentage-1440.png and -390.png.
- Required server build passed and test-output was removed.
- Independent review found no Critical or Important defects; a targeted recheck
  also accepted the responsive layout fix and fixture-only guard.
- Affected `GOMAXPROCS=2 go test -race -p 1 ./internal/accountpolicy
  ./sdk/cliproxy/auth ./internal/accountpolicyapi ./internal/api ./sdk/cliproxy`
  passed, with no race reports: /tmp/cliproxy-split-race.log.
- All changed Go files pass gofmt; staged diff passes whitespace checks.

Production verification: same container ID
95130a119c65ef0dc6137cdb06d4182c8e5affb43be223d67a84d065d976c95c,
started 2026-10-05T19:45:21.269634547Z, restart count 0. Health returned HTTP 200
on loopback, LAN and Tailscale. Main HEAD remains 17199e47 with the same preexisting
working-tree changes. No production configuration, credentials, policy journal,
listeners or serving process was modified. Candidate remains on
feature/allowance-history; no deployment or handoff is claimed.
