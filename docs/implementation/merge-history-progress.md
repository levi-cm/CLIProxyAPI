# Allowance-history merge and runtime verification

Requested integration: a35905f0 on feature/allowance-history into main, including
later accepted fixes 65992243, 27267423 and a6ad6e31. Production automatic refill
redemption must stay enabled and existing model traffic must remain uninterrupted.

An independent pre-merge review found two Important defects and reproduced both
using fixture-only Go overlays, without changing the checkout or production:

- Removing derived history/lookup metadata while leaving raw segments and valid
  segment indexes allowed old execution UUIDs to be appended twice. Missing child
  leaves and valid-empty JSON leaves caused the same issue; corrupt JSON could
  stop archival writes. Startup now compares the complete reachable lookup tree's
  bounded-leaf count/checksum with one segment index at a time, and rebuilds only
  inconsistent derived trees. Raw history is unchanged. Startup raw-index rebuilds
  defer lookup writes until reconciliation; ordinary append recovery still indexes
  uncertain synced records before retry. Malformed digest/segment entries fail
  validation. No per-append scan over historical segment indexes was added.
- Failed discovery following credential reassignment relabelled the previous
  owner's retained quota. The next success then lacked an identity_change marker
  and could join different accounts in the graph. Failed discovery now preserves
  verified evidence provenance; the next successful observation records the
  logical ownership boundary.

Both original reproductions and expanded permanent regressions pass. The reviewer
independently verified the fixes and found no remaining Critical/Important issue.
Logs: /tmp/cliproxy-merge-recovery-red.log and -recovery-green.log.

Additional verification:
- Full Go suite: /tmp/cliproxy-merge-history-all-final.log, passed.
- All 37 JavaScript tests: /tmp/cliproxy-merge-history-js.log, passed.
- Desktop/mobile dashboard, history/single/split, mandatory-automation browser
  checks: /tmp/cliproxy-merge-history-dashboard-browser.log,
  -history-browser.log and -lock-browser.log, passed. The lock browser harness now
  waits for intercepted requests before closing the page, preventing a teardown
  race from disposing an in-flight response.
- Required server build succeeded, test-output removed. Go formatting,
  JavaScript syntax and whitespace checks passed.
- Expanded race suite passed accountpolicy, accountpolicyledger, accountpolicyapi,
  auth selection, usage, executor measurement helpers, management and API packages:
  /tmp/cliproxy-merge-history-race.log. SDK initially failed three pre-existing
  fixture cases: SameRevisionWaitDoesNotWaitForOtherAuthInBatch, and the auth-file
  Replacement/Patch restoration tests. The untouched 17199e47 source reproduced
  all three (/tmp/cliproxy-merge-baseline-sdk-race.log). Review identified an
  ordering assumption in the concurrent registration hook; failed cleanup could
  also leak fixture registrations into subsequent cases. The fixture now selects
  the registration that really completed, and drains the batch before unregistering
  global clients. Production auth code is unchanged. The three tests passed under
  race three times, followed by the entire SDK package passing under race:
  /tmp/cliproxy-merge-history-sdk-race-targeted-fixed.log and -sdk-race-final.log.

Live baseline: authenticated model catalog was current (13 models). A fresh SSE
request using gpt-6-luna completed with the expected controlled response; a second
request including the returned conversation output also completed. That model
did not return encrypted reasoning in this probe, so this probe is not evidence
of an encrypted-continuation handoff. Deterministic encrypted HTTP/SSE/WebSocket
continuation coverage remains in the Go integration suites.

Production cutover gate:
- One container directly owns loopback, LAN and Tailscale port 8317. Its source
  Stop calls http.Server.Close rather than draining, so replacement closes active
  HTTP connections; service shutdown also closes persistent sessions.
- Existing durable bindings explicitly are not a multi-replica dispatch
  coordinator. A second writer against production credentials, binding state or
  policy journals is prohibited. No listener/state handoff has been verified.
- Therefore a source merge, successful build and fixture container are not a
  production rollout. Do not restart/recreate the production container, disable
  automatic resets, or claim the new version is serving production models until
  continuous access, stream/WebSocket preservation, authoritative state transfer
  and rollback have been rehearsed successfully.

The release binary/image may be staged and smoke-tested with separate fixture
configuration, mock upstream and temporary state only. Production release
activation remains blocked by this verified architectural limitation.
