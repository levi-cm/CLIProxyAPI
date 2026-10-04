# Private account pool and companion deployment

The backend policy and companion inbox are separate processes. The inbox selects
machines and limits builds; the proxy selects eligible provider accounts. This
recipe uses the host observed during implementation: Tailscale was `Running` on
`levi-thinkcentre-m700.dinosaur-dojo.ts.net`, IPv4 `100.82.251.30`. Local tools were
Codex CLI `0.160.0`, tmux `3.4`, Python 3, and systemd `255`. The checked-out base
was `8ef43e4df3b216a42493105d31c2873b69191473` (the specification's snapshot).

These are reviewed installation recipes, not a claim that host services,
remote workers, production keys, or tailnet rules have been installed. The
implementation does not redeem live credits. Keep the existing account files
and management panel; import credentials through the existing credential manager.

## Stable endpoint and private listener

| Purpose | Address |
| --- | --- |
| Tailnet origin | `http://levi-thinkcentre-m700.dinosaur-dojo.ts.net:8317` |
| Inference base | `http://levi-thinkcentre-m700.dinosaur-dojo.ts.net:8317/v1` |
| Policy API | `http://levi-thinkcentre-m700.dinosaur-dojo.ts.net:8317/v8/management/account-policy` |
| Policy UI | `http://levi-thinkcentre-m700.dinosaur-dojo.ts.net:8317/account-policy.html` |
| Existing management panel | `http://levi-thinkcentre-m700.dinosaur-dojo.ts.net:8317/management.html` |
| Private edge listener | `100.82.251.30:8317` |
| Proxy backend listener | `127.0.0.1:8318` |

Use the DNS origin everywhere, including `proxyctl` and worker profiles. HTTP
travels inside the encrypted tailnet link; it is not a public HTTP listener.
An operator may provision tailnet TLS later, updating all client origins together.
Do not enable Funnel, public port forwarding, an Internet-facing listener, or
an unauthenticated sharing service. The Caddy admin listener and automatic HTTPS
are disabled in the supplied private edge recipe.

Tailnet grants operate on ports, so a grant to inference port 8317 also reaches
management paths. The supplied [Caddyfile](../companion/Caddyfile) adds a separate
source-IP gate for both management API versions and both UI pages. Its initial
operator allowlist contains the central host only; add each approved operator's
exact Tailscale address after verifying device ownership. Workers receive
inference keys only. Never give workers the management key or allow the entire
`100.64.0.0/10` range as an operator. The backend remains loopback-only, with
`management.allow-remote: false` and no trusted forwarding proxies.

The edge uses immediate streaming flush and native WebSocket forwarding. Keep
read, write, response-header and streaming timeouts unset: Caddy's HTTP read/write
and stream timeouts default to none. Keep automatic request retries disabled so
completed tool effects are not replayed. Avoid edge reloads during live WebSocket
sessions. See [Caddy reverse proxy configuration](https://caddyserver.com/docs/caddyfile/directives/reverse_proxy).
The optional offline smoke test uses the supplied Caddyfile on temporary private
port 18417, with an in-process mock provider. On the central host, run
`CADDY_BIN=/path/to/caddy python3 -m unittest discover -s companion -v`; it verifies
HTTP, SSE, WebSocket frames/reconnect, and management rejection even with spoofed
forwarded headers. Without `CADDY_BIN`, this host-specific test is explicitly
skipped; the inbox tests still run. It does not change tailnet rules or install
a service. Caddy `2.11.7` passed this validation during implementation.

## Tailnet access policy

Merge the following into the owner's policy, replacing approved device tags and
reviewing existing broad grants. Grants are additive; an existing allow-all rule
would defeat the intended exclusion. Assign the proxy tag to the central host,
the operator tag only to approved operator devices, and the worker tag only to
provisioned workers. Check the owner's SSH policy separately before remote setup.

```json
{
  "tagOwners": {
    "tag:cliproxy-proxy": ["autogroup:owner"],
    "tag:cliproxy-operator": ["autogroup:owner"],
    "tag:cliproxy-worker": ["autogroup:owner"]
  },
  "grants": [
    {
      "src": ["tag:cliproxy-operator", "tag:cliproxy-worker"],
      "dst": ["tag:cliproxy-proxy"],
      "ip": ["tcp:8317"]
    }
  ]
}
```

Tag ownership determines who can assign tags; it is not itself a network grant.
The network grant above restricts device access, while Caddy restricts management
paths to operator addresses and the proxy requires separate authentication.
Validate positive and negative access cases in the tailnet console before
applying the merged policy. See [Tailscale grants syntax](https://tailscale.com/docs/reference/syntax/grants).
No ACL or tag mutation is performed by the implementation.

## Install the persistent proxy

Use Go 1.26 or newer. Build and retain the previous binary before deploying:

```bash
go build -o cli-proxy-api ./cmd/server
go build -o proxyctl ./cmd/proxyctl
python3 -m unittest discover -s companion -v
```

Provision dedicated `cliproxyapi`, `cliproxy-worker`, and `caddy` service users.
Install Caddy using its official Linux package. Run the following as the local
administrator only after reviewing paths and preserving existing installation
files; these commands install recipes and do not overwrite the normal Codex home:

```bash
install -d -m 0755 /opt/cliproxyapi /opt/cliproxyapi/companion
install -m 0755 cli-proxy-api proxyctl /opt/cliproxyapi/
install -m 0644 companion/fleetctl.py /opt/cliproxyapi/companion/
install -d -o cliproxyapi -g cliproxyapi -m 0700 /etc/cliproxyapi /var/lib/cliproxyapi
install -d -o cliproxyapi -g cliproxyapi -m 0700 /var/lib/cliproxyapi/auths /var/lib/cliproxyapi/account-policy
install -d -o root -g caddy -m 0750 /etc/cliproxyapi-edge
install -o root -g caddy -m 0640 companion/Caddyfile /etc/cliproxyapi-edge/Caddyfile
install -m 0644 companion/systemd/cliproxyapi.service companion/systemd/cliproxy-edge.service /etc/systemd/system/
```

Create `/etc/cliproxyapi/config.yaml` from the complete `config.example.yaml`,
merging [the deployment overlay](../companion/proxy.config.example.yaml). Set
owner `cliproxyapi:cliproxyapi`, mode `0600`; its directory is `0700`. Generate
different random inference and management keys locally. Assign a separate
inference key per worker for revocation. Do not put production configuration,
OAuth files, profiles containing keys, or the inbox database into git.
The daemon needs write access to its configuration directory because authenticated
management edits and initial key hashing can save configuration. Application logs
rotate at 64 MB total in the working directory, with request bodies and debug
logging disabled. Systemd captures service lifecycle output in the journal;
configure the host's journal retention policy as needed.

Update the Caddy operator allowlist, then validate and start:

```bash
caddy validate --config /etc/cliproxyapi-edge/Caddyfile --adapter caddyfile
systemd-analyze verify /etc/systemd/system/cliproxyapi.service /etc/systemd/system/cliproxy-edge.service
systemctl daemon-reload
systemctl enable --now cliproxyapi.service cliproxy-edge.service
systemctl status cliproxyapi.service cliproxy-edge.service
ss -ltnp 'sport = :8317 or sport = :8318'
tailscale serve status
tailscale funnel status
```

Expected listeners are exactly `100.82.251.30:8317` and `127.0.0.1:8318`, not
`0.0.0.0`, `[::]`, or a public/LAN address. Inspect existing Tailscale forwarding
and public firewall/router rules; the recipe creates no forwarding service.
The `--local-model` service flag deliberately avoids remote model catalogue
updates; remove it only if those updates are desired. `Restart=on-failure` keeps
the proxy running after a crash. Its discovery/reset lifecycle runs in the proxy
service and is independent of the owner's browser or laptop connection.

Start with `account-policy.enabled: false` and `automation: off`. Opt into dashboard
collection with `account-policy.observations-enabled: true` (config controls it on
restart), verify account/workspace mapping and fresh evidence, then
enable deadline routing, inspect dry-run advice with `notify`, and explicitly
choose `auto_expiring` only after review. Run one writer instance; a second proxy
must use read-only reset policy unless shared ownership has been implemented.
Keep saved policy state and operation journals on the protected local disk.

## Isolated direct and proxy Codex access

The installed CLI uses named profile *files*, selected by `--profile`: for
example `$CODEX_HOME/proxy.config.toml`. Do not assume legacy `[profiles.proxy]`
tables work. Provider configuration belongs in user-level configuration, not
project `.codex/config.toml`. This was checked against local `codex exec --help`
and [the official configuration reference](https://learn.chatgpt.com/docs/config-file/config-reference).

Keep the owner's normal direct profile and sign-in unchanged. Create a separate
home such as `/var/lib/cliproxy-fleet/codex-proxy` with mode `0700`, and copy
[proxy.config.toml](../companion/proxy.config.toml) into it. It contains no model
substitution and no key. Choose the exact model explicitly after checking `/v1/models`.
The custom provider uses the Responses protocol and `CLIPROXY_API_KEY`, and can
request WebSocket transport. That capability flag alone does not prove transport.
CLI 0.160.0 rejects `--profile` for `app-server`; an app-server host must load this
provider in its dedicated user-level `config.toml` instead. Offline
`app-server config/read` verified these provider fields without inference.

```bash
# Existing direct account/profile:
codex --model YOUR_APPROVED_MODEL

# Isolated proxy account/profile; load CLIPROXY_API_KEY from a protected local file.
env CODEX_HOME=/var/lib/cliproxy-fleet/codex-proxy codex --strict-config --profile proxy --model YOUR_APPROVED_MODEL
```

For service workers use `/etc/cliproxy-worker/proxy.env`, mode `0600`, containing
only `CLIPROXY_API_KEY` and the dedicated `CODEX_HOME` path; never a management key.
The worker service user owns this Codex home. The direct and proxy homes do not
share login files. Do not copy OAuth files into the proxy client home.

Operator CLI setup uses `CLIPROXY_URL` for the origin and
`CLIPROXY_MANAGEMENT_KEY_FILE` for a local protected key file:

```bash
export CLIPROXY_URL=http://levi-thinkcentre-m700.dinosaur-dojo.ts.net:8317
export CLIPROXY_MANAGEMENT_KEY_FILE=/etc/cliproxy-operator/management.key
proxyctl accounts
proxyctl explain
proxyctl refresh ACCOUNT_CREDENTIAL_ID
proxyctl resets list
# The following are real writes. Review account and credit IDs before running:
proxyctl resets schedule ACCOUNT_CREDENTIAL_ID CREDIT_ID 2026-10-05T06:08:00+02:00
proxyctl resets cancel SCHEDULE_ID
proxyctl resets redeem ACCOUNT_CREDENTIAL_ID CREDIT_ID
```

The sample schedule is illustrative, not a provider expiry or an approval to
redeem. UTC instants are stored by the proxy. Europe/Berlin changes offset in
October; verify each expiry's displayed offset rather than reinterpreting a
fixed `GMT+2` label. Local cooldown clearing is a distinct operation and never
redeems a provider credit.

## Companion inbox and capacity

[fleet.example.json](../companion/fleet.example.json) is the small fleet inventory
to keep in a separate setup checkout or reuse from this directory. It records
the stable endpoint, worker identity, tools, and limits, without secrets.
Inventory is not a claim that a worker is available. Register measured capacity
with `fleetctl`, then refresh its heartbeat; a worker older than 120 seconds or
disabled is excluded. Selection prefers lowest occupancy and stable ID.

The executable companion uses only Python's standard library and SQLite. Run
one coordinator database on local disk; invoke it over SSH when coordinating
remote machines. Do not put SQLite on NFS or run independent databases claiming
the same global capacity. SQLite immediate transactions make claims atomic,
including both task capacity and separately bounded build capacity. The default
inventory allows two tasks and one build on the central host; adjust to measured
RAM/CPU. Mark any agent job that may build as `--build`, reserving its build slot
for the whole job, or wrap individual builds in separately claimed build tasks.

Every task uses the root of a distinct linked Git worktree. The runner checks
this at submission and claim time and prohibits two working tasks in one
worktree. It accepts an explicit argv JSON array, without implicit shell parsing.
Tasks start `needs_input`, require `approve`, and become `queued` until claimed.
Successful command exit becomes `review_ready`, never automatic `settled`.

```bash
python3 companion/fleetctl.py --db /var/lib/cliproxy-fleet/inbox.sqlite worker levi-thinkcentre-m700 --capacity 2 --build-capacity 1
python3 companion/fleetctl.py --db /var/lib/cliproxy-fleet/inbox.sqlite heartbeat levi-thinkcentre-m700
python3 companion/fleetctl.py --db /var/lib/cliproxy-fleet/inbox.sqlite pick --build
git worktree add -b task/deadline-tests /var/lib/cliproxy-fleet/worktrees/deadline-tests HEAD
python3 companion/fleetctl.py --db /var/lib/cliproxy-fleet/inbox.sqlite add --title 'Verify deadline fixtures' --cwd /var/lib/cliproxy-fleet/worktrees/deadline-tests --build --argv '["go","test","./internal/accountpolicy"]'
# Replace TASK_ID using the returned JSON. Approval covers this exact argv/cwd.
python3 companion/fleetctl.py --db /var/lib/cliproxy-fleet/inbox.sqlite approve TASK_ID
python3 companion/fleetctl.py --db /var/lib/cliproxy-fleet/inbox.sqlite run levi-thinkcentre-m700 --max-tasks 4
python3 companion/fleetctl.py --db /var/lib/cliproxy-fleet/inbox.sqlite list --search deadline
python3 companion/fleetctl.py --db /var/lib/cliproxy-fleet/inbox.sqlite state TASK_ID settled
python3 companion/fleetctl.py --db /var/lib/cliproxy-fleet/inbox.sqlite archive TASK_ID
python3 companion/fleetctl.py --db /var/lib/cliproxy-fleet/inbox.sqlite unarchive TASK_ID
python3 companion/fleetctl.py --db /var/lib/cliproxy-fleet/inbox.sqlite snooze TASK_ID 2026-10-06T09:00:00+02:00
python3 companion/fleetctl.py --db /var/lib/cliproxy-fleet/inbox.sqlite list --all
```

The coordinator database directory must belong to its user and have mode `0700`;
the CLI creates a private directory when missing and refuses an existing public
directory. Database, WAL/SHM files, and per-task stdout/stderr logs are private.
Task titles, messages, argv and transcripts may contain work content: keep them
outside git, limit operator access, and rotate/archive task logs according to the
owner's retention policy. The CLI itself emits task JSON and sanitized errors;
subprocess output remains in private logs. There is no inference request polling,
credit redemption, HTTP server, or provider account scheduler in the companion.

## Durable remote work and an approved overnight backlog

For a persistent central worker, provision its user/directories and copy
[cliproxy-fleet.service](../companion/systemd/cliproxy-fleet.service). Review the
argv and completion criteria of at most four approved tasks, register the worker,
then `systemctl start --no-block cliproxy-fleet.service`. The oneshot service
continues after UI or SSH disconnect and stops after its bounded approved backlog.
It has no periodic timer or automatic idle token consumption. Use an explicit
per-job model/token budget in the native agent configuration when available.

For another worker, verify required tools and tailnet access first, create its
worktree on that machine, and add its identity to the fleet inventory. Use the
central coordinator's `pick`, then `claim TASK_ID REMOTE_WORKER_ID` to reserve
capacity before starting the exact approved task remotely. The central task cwd
is the coordinator's isolated task contract checkout; the remote worker gets a
separate checkout at a reviewed path. The command runner is local: it does not
pretend an inventory hostname is an SSH execution backend. Use the owner's
existing remote agent tool, a remote systemd unit, or tmux to run the reviewed
remote command and report its exit/CI evidence to the central inbox.

```bash
# On the provisioned remote host, using its dedicated proxy profile:
tmux new-session -d -s task-deadline 'env CODEX_HOME=/var/lib/cliproxy-fleet/codex-proxy codex exec --strict-config --profile proxy --model YOUR_APPROVED_MODEL --sandbox workspace-write -C /var/lib/cliproxy-fleet/worktrees/deadline-tests --json "Implement the approved deadline-tests work package and report verification."'
tmux attach-session -t task-deadline
# Detach with Ctrl-b d. Reattach after the UI/SSH connection returns.
```

Load the inference key into the dedicated tmux/server environment from the
protected worker file before launch. A new remote host has no enabled identity
until the operator provisions it. For CI offload, claim a build task against that
worker's capacity and run its CI there; do not start an untracked second build.
Refresh the central heartbeat from actual worker/tool observations, not a timer
that makes a dead host appear online.

A worker's own inference connection failure is different from the UI device
disconnect. Keep interrupted jobs `needs_input` until the native client can
reconcile continuation, completed tool effects, and account affinity. Do not
restart the entire task as a fabricated recovery. After coordinator/process
restart, `working` claims remain reserved; inspect remote processes and outputs
before marking `needs_input` or `failed`. Only explicit reapproval retries them.
Do not release a claim while its process is still executing.

## Pending messages, steering, implementation and review

Queue a pending message with `fleetctl message TASK_ID 'Review account ownership'`.
For an exec job the verified safe boundary is completion of its command/turn.
Once the task leaves `working`, `fleetctl boundary TASK_ID` returns and marks
pending messages delivered; `fleetctl messages TASK_ID` retains the message
history for recovery if the receiver disconnects. Pass returned input to a
native resumed turn. The companion rejects boundary delivery during a working
exec job and never injects terminal keystrokes or sends OS signals as steering.

When the owner's existing tool hosts Codex app-server, use its initialized,
authenticated connection and current thread/turn IDs for supported immediate
control. `turn/steer` appends input to the active turn and checks `expectedTurnId`.
For explicit interrupt-and-steer, call `turn/interrupt`, wait for completion,
then start a new turn with the revised input. These are native client operations,
not commands the inbox claims to implement. See
[the official app-server guide](https://developers.openai.com/codex/app-server).

```json
{"id":32,"method":"turn/steer","params":{"threadId":"ACTUAL_THREAD_ID","expectedTurnId":"ACTUAL_TURN_ID","input":[{"type":"text","text":"Prioritize the failing tests."}]}}
```

```json
{"id":33,"method":"turn/interrupt","params":{"threadId":"ACTUAL_THREAD_ID","turnId":"ACTUAL_TURN_ID"}}
```

Give each implementation work package explicit owned paths, frozen contracts,
acceptance cases, model, budget, and rollback instructions. Use a fresh review
thread and a separate review worktree at the candidate commit; never share
uncommitted files with a concurrent reviewer. Run `codex exec review --model YOUR_APPROVED_MODEL --commit SHA`
or app-server detached review in that context. `review_ready` means awaiting this
review, test evidence and operator acceptance; only then mark `settled`.

For UI work, start a loopback fixture preview in the implementation worktree,
verify it with Playwright, and attach screenshots/traces to review. Any remote
preview requires a separate private tailnet route and operator restriction;
do not expose a public preview or reuse the inference key as preview auth.
Create a draft PR only when publishing is authorized, describing final behavior,
test commands and material limits. Inspect CI logs, repair scoped failures in
the implementation worktree, rerun relevant checks, and request fresh review.
If the native client cannot migrate an unsafe continuation to an expiring account,
use only independently approved backlog jobs as new sessions; report unused
allowance instead of generating meaningless work.

## Tailscale transport and disconnect verification

Use an approved operator and an approved worker after provisioning. Obtain keys
from protected files without putting them in URLs, process arguments, screenshots
or terminal transcripts. From each client check DNS, tailnet and listener before
diagnosing provider errors:

```bash
getent hosts levi-thinkcentre-m700.dinosaur-dojo.ts.net
tailscale status
tailscale ping levi-thinkcentre-m700.dinosaur-dojo.ts.net
```

Use `curl --config /etc/cliproxy-worker/curl.conf` where the mode-0600 file supplies
`header = "Authorization: Bearer ACTUAL_INFERENCE_KEY"`. No live credit consumption
belongs in a transport smoke test. Use a reviewed model and a small real request
only when inference has been approved; otherwise use the isolated mock provider
transport fixtures from the test suite.

```bash
curl --config /etc/cliproxy-worker/curl.conf --fail-with-body http://levi-thinkcentre-m700.dinosaur-dojo.ts.net:8317/v1/models
curl --config /etc/cliproxy-worker/curl.conf --fail-with-body -H 'Content-Type: application/json' -d '{"model":"YOUR_APPROVED_MODEL","input":"Reply with OK.","stream":false}' http://levi-thinkcentre-m700.dinosaur-dojo.ts.net:8317/v1/responses
curl --config /etc/cliproxy-worker/curl.conf --fail-with-body --no-buffer -H 'Content-Type: application/json' -d '{"model":"YOUR_APPROVED_MODEL","input":"Reply with OK.","stream":true}' http://levi-thinkcentre-m700.dinosaur-dojo.ts.net:8317/v1/responses
```

For WebSocket use the isolated proxy Codex profile and inspect sanitized proxy
transport diagnostics plus the client's event output. An HTTP 101 upgrade proves
only the downstream leg. Check the selected credential's actual upstream transport
and enabled WebSocket capability too. Exercise normal completion, the same
connection's next request, reconnect, compatible HTTP fallback, tool calls,
compact and safe/unsafe continuation fixtures. Keep a fixture stream open past
normal edge idle intervals, disconnect the operator UI, and verify the worker,
proxy service, discovery and scheduler continue. Disconnect the worker's own
tailnet separately and verify native recovery without duplicated effects.

From an operator, verify UI and `proxyctl accounts`; from a worker, verify inference
succeeds but management/UI return edge `403` even if it supplies the management
key. From an unapproved device, connection must fail before HTTP. A wrong
inference key should return proxy `401`; wrong management key from an approved
operator should return management authentication failure. Distinguish DNS or
tailnet connectivity, edge `403`, listener refusal, proxy authentication failure,
upstream authentication rejection, upstream quota/cooldown, and schema/identity
discovery failure in the incident report. Do not diagnose all of them as quota.

These production access, rejection, long-stream and disconnect cases require
operator device identities, keys and remote host access. They cannot be certified
from source validation or from the local host's self-connection alone.

## Rollback and evidence

First set automation to `off`, leave discovery running, and inspect unresolved
operations. Disable the optional policy to restore the configured existing
selector. Stop new companion dispatch; keep the inbox and task worktrees. Preserve
the auth directory, policy snapshots and pending-operation journal before replacing
a binary. Never delete an `outcome_unknown` operation to force a fresh request ID.
Retain the compatible candidate state until pending operations are reconciled.

Restore the previous binary/config only after checking state compatibility and
stopping the service cleanly; restart and check account identity, pending operations,
cooldowns, listener addresses, tailnet access and HTTP/SSE/WebSocket behavior.
Revert source commits with `git revert` in a reviewed branch. Preserve the existing
management panel and keep client direct profiles available. Archive the completed
task rather than deleting its recovery evidence.

Repository verification covers the companion tests, configuration parsing, service
recipe syntax, isolated proxy compilation and deterministic backend/UI fixtures.
Record actual commands/results in the release handoff. It does not certify live
provider reset semantics, purchased credits, production ACLs, or unprovisioned
remote hosts.
