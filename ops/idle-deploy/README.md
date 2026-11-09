# Idle deployment and independent Codex verification

This is a one-shot, explicitly staged deployment controller. It never pulls or
merges upstream automatically. It deploys only the immutable Docker image and
source revision in its private release manifest.

The user authorized a five-minute period with no model API requests and a target
below 40 seconds of maintenance, including rollback. This task-specific budget
allows a short interruption; it does not make the existing router capable of a
zero-downtime listener handoff. Existing requests, SSE streams and WebSocket
connections must finish before the serving writer can stop.

## Prepare, launch, inspect

Run from this isolated checkout, with all artifacts inside the project:

```bash
sudo -n python3 ops/idle-deploy/main.py prepare \
  --image cliproxyapi-fork:metadata-6be26d0e \
  --commit 6be26d0ef413437cdaff5ab1b7e79205dbc5851f
sudo -n python3 ops/idle-deploy/main.py launch \
  --release /DATA/AppData/CLIProxyAPI/.tmp/idle-deploy/RELEASE/release.json
sudo -n python3 ops/idle-deploy/main.py status \
  --release /DATA/AppData/CLIProxyAPI/.tmp/idle-deploy/RELEASE/release.json
```

Preparation copies protected configuration, credentials and state to private
fixture directories. Every test writer mounts those copies in a network-isolated
container. Production data is never mounted into a running test container.
A final production candidate may be pre-created with its real mounts, but stays
stopped until the old writer has exited and every ownership lock is free.

Preparation measures candidate and rollback startup, preserved owner state,
existing-connection retention at the admission gate, a full actual cutover, and
an independently triggered watchdog rollback against candidate-written fixture
state. Complete host gate code runs on a reserved unused host port; namespace
rules use the fixture's real port. No production port is gated in a rehearsal.
Preparation fails if configuration drifted or measured recovery leaves too little
margin. The fixture and private backups retain their existing credential access
restrictions. They are excluded from Git and Docker build contexts.

Launch creates a background systemd service named `cliproxy-idle-deploy-RELEASE`.
It waits for up to seven days and performs at most one deployment. Live status and
verification reports are in
`/DATA/AppData/CLIProxyAPI/output/verification/idle-deploy/RELEASE/`.
To cancel while still waiting, stop that exact service. The separate watchdog
retains responsibility for recovery if a cutover was already in progress.

When the operator explicitly requests deployment now, add `--now` to `launch`.
This skips the five-minute quiet period. Fresh healthy capture, no pending parser
state, zero active requests/connections, final admission/drain, writer fencing,
the measured 40-second budget and independent rollback watchdog remain required.

## Retire dormant client connections after the idle window

The default mode waits for every client connection to close naturally. When an
operator authorizes retirement of dormant client sockets, pin that policy during
preparation:

```bash
sudo -n python3 ops/idle-deploy/main.py prepare \
  --image cli-proxyapi-fork:backport-7bcd5867 \
  --commit 7bcd5867d9250f50ba8a25107cdea9a0f7cb6359 \
  --retire-idle-clients
```

Launch the resulting release normally, without `--now`. Retirement is stored in
the private manifest and exercised in both successful and forced-rollback fixture
rehearsals with a real idle client socket. It cannot be enabled as a launch flag.
The five-minute observed quiet window and zero authoritative active requests are
still required. After admission closes, existing client input is frozen, active
requests are checked again, and only then are dormant sockets retired, excluding
the exact pre-established control connection. Clients may need to reconnect;
durable conversation ownership, credentials, and journals remain in place.

The final drain still requires fresh healthy capture, no incomplete parser state,
zero active leases, and no remaining client connections. Intentional retirement
packets do not start another five-minute timer after all client input is frozen.
No monitoring exception applies to unknown, incomplete, upgraded, body-bearing,
or ambiguously sequenced traffic.

A fully parsed monitoring-only connection may end with a matching empty TCP reset
without resetting the idle window. A reset from unknown or business traffic still
resets it. Status includes `idle_required_seconds`, `retire_idle_clients`, capture
drop counts, and a persistent `last_block` with sanitized reason/counts. A normal
status heartbeat no longer hides the last deployment gate failure.

## Interrupting cutover (operator-authorized)

Production traffic may never reach zero. When the operator explicitly authorizes
interrupting in-flight requests, SSE streams and WebSockets, deploy a prepared
release with the separate interrupt controller instead of `main.py launch`:

```bash
sudo -n python3 ops/idle-deploy/main.py prepare \
  --image IMAGE --commit COMMIT --retire-idle-clients
sudo -n python3 ops/idle-deploy/interrupt.py launch \
  /DATA/AppData/CLIProxyAPI/.tmp/idle-deploy/RELEASE/release.json
```

`launch` starts the worker in the transient unit `cliproxy-idle-deploy-RELEASE`
(`KillMode=control-group`) and prints JSON; the worker cuts over immediately.
The release must be prepared with this controller version (`interrupt.py` hash
pinned) and less than 24 hours ago. `--retire-idle-clients` is recommended so the
fixture rehearsals exercise the same client freeze and `ss -K` path.

Only the idle window, capture evidence and zero-activity/connection requirements
are removed. Everything else is the unchanged cutover: container, candidate,
config/.env, rehearsal and controller identity; measured timing budget;
independent watchdog and rescue; admission gates; writer fencing; readiness;
state preservation; `.env` image commit; ungating; journaling; `deployment.json`
and Codex verification. The old container must not use host networking.

- Automatic reset redemption is never interrupted. `redemption.lock` is taken
  before any other step; an in-flight redemption is awaited for up to 120 seconds,
  after which the attempt blocks without touching production.
- After admission closes, client input is frozen (control socket excepted), the
  authoritative active-request count and client connection count are journaled as
  `interrupted_requests` / `interrupted_connections`, and every other client TCP
  socket on the router ports is killed with `ss -K` in the old namespace.
- If client sockets remain after about three seconds, the attempt blocks before
  stop and the original is retained. The pre-stop window is six seconds and
  preflight refuses releases whose measured budget cannot absorb it.
- The journal and `failure.json` carry `mode: interrupt`. Interrupted clients must
  reconnect or retry; durable conversation ownership, credentials and journals are
  retained exactly as in the default mode.

## Idle and preservation rules

- A passive IPv4/IPv6 observer runs in the serving network namespace. It sends no
  API requests. Only a bounded request line and parser counters are retained;
  credentials, header values and business bodies are not logged or saved.
- Positively identified GET/HEAD health, model-list and management-read requests
  do not reset the timer. POSTs, model API calls, WebSocket upgrades, unknown
  protocols, malformed framing, reassembly uncertainty and capture drops do.
- Five minutes must be observed after the watcher starts. A stale/dead observer,
  incomplete request, active request lease or open/pending client connection
  blocks deployment in the default mode. Idle WebSockets close naturally unless
  dormant socket retirement was explicitly pinned during preparation.
- The controller rechecks the exact container, config/environment hashes, image,
  source identity, fresh rehearsal and controller file hashes. It never silently
  replaces a changed release or live configuration.
- Admission closes across loopback/LAN/Tailscale, host INPUT/OUTPUT/Docker traffic,
  root clients, IPv4/IPv6 and the old namespace. Existing connections are not
  reset. A final drain and fresh quiet observation happen before any stop.
- Both `owner.lock` and `history/writer.lock` are required; `redemption.lock` is
  briefly fenced only after an in-flight redemption has completed. Automatic
  redemption stays enabled. No policy mode or readonly switch is used to pause it.
- All three writer locks must be free after the old process exits. Readiness
  requires the reviewed source, working authenticated models/history, writable
  authority and mandatory `auto_expiring` settings with the original expiry guard.
- Config YAML, credentials, bindings and policy journals stay in place. Only the
  deployment image variable in `.env` changes in place after readiness. Rollback
  retains current durable state and never restores stale credential/journal copies.
- Existing binding owners, credential files and raw-history identity/length are
  checked before admission reopens. Full retained-history prefix hashes are
  compared afterward without extending the outage.

## Failure and timing behavior

An independent systemd watchdog has explicit 50ms timer accuracy, no randomized
extra delay and its own cgroup. Candidate readiness cannot consume the measured
rollback reserve. It kills only the stuck deployment worker, confirms exclusive
worker ownership and rereads the durable transaction before recovery.

Commit and rescue share a lock. A committed, restored or retained serving writer
is a terminal outcome: later firewall-cleanup retries never stop it again. Rule
cleanup uses bounded xtables waits, checks errors, and positively verifies both
host rule sets before recording the interruption as finished. A transient cleanup
failure leaves the independent rescue service retrying.

The entire interruption is measured from admission gating to confirmed ungating.
The target below 40 seconds is supported by complete isolated rehearsals and a
recovery reserve; arbitrary host/kernel/Docker failure cannot be promised a fixed
recovery time. Every actual measured result is retained rather than reported as
zero downtime.

## Post-deployment verification

After reopening admission, deterministic real checks verify all interfaces, a
genuine pre-deployment encrypted continuation on its same durable owner, complete
SSE, standard-client output, two native WebSocket turns, history and mandatory
refills. Opaque continuation data stays in the private release directory.

The controller then launches a fresh ephemeral Codex session using the existing
`cod` saved-ChatGPT-login entrypoint. That verifier is independent of the router's
model provider, so a broken router cannot prevent the verification session from
starting. Its writable workspace is the report directory, with network access
for read-only probes. Production mutations, restarts, refills and credential
printing are explicitly forbidden. It independently checks the live commit and
interfaces and reviews sanitized protocol/ownership evidence. Its JSON verdict
and private session log are retained.

Failed or inconclusive extended verification is reported explicitly. It never
causes an immediate restart after clients have been readmitted. The narrow
pre-admission readiness failure path is the automatic rollback path; a later
repair or rollback requires another safely drained release.

## Validation

```bash
TMPDIR=/DATA/AppData/CLIProxyAPI/.tmp \
  python3 -m unittest discover -s ops/idle-deploy -p 'test_*.py' -v
```

Synthetic tests exercise traffic privacy/classification, malformed and fragmented
HTTP, IPv4/IPv6, capture uncertainty, model-vs-health behavior, idle freshness,
writer exclusion, cutover ordering, rollback failure phases, watchdog commit races,
xtables errors, surviving gates and terminal recovery idempotency. Full isolated
Docker/systemd rehearsals are required separately before a release can be armed.
