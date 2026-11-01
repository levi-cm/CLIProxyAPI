# Idle deployment blocker fixes

The idle controller previously restarted its quiet timer when a completed health
probe disconnected with TCP RST. Its preflight also required every TCP client to
close naturally, so dormant clients could indefinitely prevent deployment despite
hours without completed model requests. Its next status heartbeat hid gate failures.

The controller under `ops/idle-deploy` is imported from the existing controller at
`22b9becb` and fixed on the backport branch. No application, routing, reset,
credential, journal, or conversation-binding implementation changes are included.

## Behavior

- Ignore only an empty, correctly sequenced RST on a fully parsed monitoring-only
  connection. Unknown, partial, upgraded, body-bearing, and business connections
  continue to reset the timer. No packet bodies or header values are retained.
- Persist sanitized blocking reasons and counts in `last_block`, alongside the
  required idle duration, retirement policy, and capture-drop count.
- Pin `--retire-idle-clients` during preparation. Normal launch still waits for
  five quiet minutes and zero authoritative active requests. It freezes client
  input, rechecks leases, retires dormant sockets except the verified control
  connection, and proves final drain before stopping the old writer. Dormant
  clients reconnect; durable owners remain intact. Existing active requests block.
- Expected retirement packets do not restart the quiet interval after client
  input is frozen. Fresh healthy capture, complete parsing, final zero leases,
  exclusive state ownership, mandatory resets, and watchdog rollback stay required.

## Validation

- The monitoring reset defect was reproduced with 600 simulated seconds and zero
  model requests. The original never became ready; the corrected observer does.
- All 83 controller tests pass, including active leases before/after the freeze,
  quiet-window changes, capture failure/pending/staleness, and persistent status.
- Passive observation of the corrected observer ignored two real health-check
  resets; unknown and business resets remained activity.
- A full rehearsal caught a fixture preflight signature mismatch before any
  production action. A regression was added and the mismatch corrected.
- Fresh candidate and rollback rehearsals preserved all 1,621 copied owners and
  mandatory automatic resets. Real dormant client sockets were retired in both
  complete cycles: success interruption 2.995 seconds; independent forced-watchdog
  rollback interruption 9.268 seconds, including 4.090 seconds recovery. These are
  isolated measurements, not actual production interruption or a future guarantee.
- Required server compilation passed. The application image remains the already
  tested immutable `7bcd5867` image; only host-side controller behavior changed.

Replacement release: `dc56bd19a6`, with a fresh manifest, controller hashes, copied
state rehearsal, rollback reserve, and encrypted-continuation preflight proof.
It supersedes waiting release `b1f2d9a236`. Arming and later automatic deployment
are distinct; inspect its live reports before claiming the application was deployed.
