# Automatic deployment self-probe failure

## Cause and evidence

The prepared release `dc56bd19a6` recorded 108 `original_retained` attempts. The
serving writer was retained, but attempts briefly gated admission before aborting.
The generic `RuntimeError` report did not preserve the underlying abort location.
The release subsequently also exceeded its 24-hour rehearsal validity.

The controller's readiness probe requests
`GET /v1/account-policy/usage-history/status`. That exact read-only endpoint was
missing from the packet observer's monitoring allowlist. The controller therefore
reset its own five-minute idle timer immediately before the retirement drain.
The original fixture used synthetic healthy/quiet evidence and could not detect
this interaction. The previous health-check RST fix did not cover this path.

The real readiness-probe function reproduced the idle-window invalidation in a
regression test. A separate source-only negative controller restored the old
classification, then ran with actual namespace packet capture against isolated
fixture state. It raised `readiness probes invalidated the fixture quiet window`
before closing admission. The corrected controller passed the same full rehearsal.

## Fix

- Classify only the exact read-only history-status endpoint as monitoring. POSTs,
  request bodies, unknown neighbour paths, protocol ambiguity, and capture loss
  remain business activity or fail-closed evidence.
- Run full fixture preflight probes through actual namespace packet capture.
  Only the isolated fixture's initial known quiet interval is seeded to avoid a
  five-minute test delay. Later activity is real observer evidence and is never
  mapped back to the initial baseline. A fresh post-probe heartbeat is required.
- Record sanitized failure reasons, exception class and source file/line/function
  in `failure.json`, the journal, and persistent status. Raw exception messages,
  headers, bodies, and credentials are not copied into these public diagnostics.
- Report expired/changed rehearsals, controller drift and config drift explicitly
  as requiring restaging. Unexpected failures also stop rather than repeatedly
  closing admission with an unexplained RuntimeError. Normal activity/drain races
  remain retryable and preserve the original writer.

## Validation and replacement release

All 88 controller tests and the required server build passed. A fresh rehearsal
preserved 1,752 owners for both candidate and rollback, with mandatory resets and
history healthy. Actual capture, readiness probes, dormant-socket retirement and
watchdog recovery were exercised. Isolated complete-cycle interruptions were
3.812 seconds for success and 10.332 seconds for forced rollback, including 5.155
seconds of recovery. These are fixture measurements, not a measured live outage
or a future timing guarantee.

Fresh release: `2b929bfd48`. The application image remains the tested immutable
`7bcd5867` image. The five-minute observation, zero active requests, guarded idle
retirement, final drain, single-writer locks and rollback reserve remain required.
The expired watcher was stopped without restarting the serving router. Arming
this replacement is distinct from a committed automatic application deployment;
its live reports are authoritative for that later result.
