# Prompt: show Claude reset grants and automatic resets in usage-tracker

Work in the `levi-cm/usage-tracker` repository (`/DATA/AppData/usage-tracker` on
debdev; Go + SQLite, no frontend build step). Read `README.md`, `PRODUCT.md` and
`IMPLEMENTATION_PLAN.md` first. Their rules on read-only access,
unknown-stays-unknown, freshness and privacy apply unchanged. Plan before
editing, keep changes small, and do not deploy without the owner's explicit
go-ahead.

## Goal

Claude Max accounts hold **reset grants**:

- a monthly "full limit reset" that clears the weekly window and the Fable window;
- one or two "Fable resets" that clear only the Fable weekly window.

CLIProxy now spends these grants automatically. It spends one when the whole
Claude fleet runs out of the allowance the grant restores and requests are
refused for it, or shortly before a grant would expire unused. Integrate this into
the existing Claude views so that, at a glance, the owner can see:

1. how many resets each account still holds, of which kind, and when each expires;
2. whether automatic resets are active, and why CLIProxy is or is not spending
   one right now ("fleet has 103 points of weekly allowance open", "waiting for
   demand", "spending");
3. the history of automatic resets (time, account, grant, trigger, outcome);
4. the effect on capacity: a usable weekly grant is worth up to one more week of
   that account's allowance, and a Fable grant up to one more week of its Fable
   allowance.

Integrate this into what exists, such as the Claude pool card, the credential
table and the per-account cards. Do not bolt on a separate dashboard.

## Terms

Keep these distinct in code, labels and docs:

- **Weekly allowance / Fable allowance:** the existing quota windows.
- **Reset grants (new, Claude):** the provider's banked resets. Name the UI
  concept **Resets** ("2 resets left", "Weekly reset", "Fable reset"). They are
  not Codex reset credits, and not a credit balance.

## Upstream contract (no new endpoints)

Use only `GET /v8/management/account-policy/accounts`, which is already in the
read-only contract. CLIProxy builds with this feature add a top-level
`claude_resets` object (the same object also appears in `/diagnostics`). Older builds omit it. If it is absent, hide the feature
or show "Not reported by CLIProxy", never 0. Every time field may be absent,
which means unknown.

```jsonc
{
  "claude_resets": {
    "mode": "auto",                 // auto | off
    "active": true,                 // CLIProxy may spend grants now
    "inactive_reason": "",          // off | policy_disabled | read_only | not_writer | journal_unavailable
    "evaluated_at": "2026-11-20T22:10:00Z",
    "weekly": {                     // need trigger for weekly grants
      "headroom": 103.0,            // open weekly allowance across the fleet, in points (100 = one account's week)
      "threshold": 5,               // CLIProxy spends a weekly grant below this
      "demand_at": "2026-11-20T22:09:58Z", // latest request refused because of this window; absent when none
      "state": "ok",                // ok | waiting_demand | settling | capped | no_grant | spending
      "reason": "103 points of allowance are still open across the fleet"
    },
    "fable": { /* same shape, for the Fable window and Fable grants */ },
    "planned": {                    // absent unless a grant is about to be spent
      "account_id": "<raw credential id>", "grant_id": "full-reset-2611",
      "class": "weekly", "trigger": "weekly_need", "reason": "…"
    },
    "accounts": {                   // keyed by RAW credential id, Claude accounts only
      "<raw credential id>": {
        "observed_at": "2026-11-20T22:05:00Z", // last grant inventory read
        "fresh": true,              // weekly usage evidence at most 15 min old
        "healthy": true,            // enabled, authenticated, not on hold and not excluded by a force-account override
        "pending": false,           // an automatic claim on this account is unresolved
        "error": "",                // sanitised; "Provider observation failed." when set
        "eligible": true,           // provider reset program eligibility
        "ineligible_reason": "",
        "at_limit": false,
        // Each window carries its own observation time.
        "weekly":  { "known": true, "used": 90, "reset_at": "2026-11-25T14:00:00Z", "observed_at": "2026-11-20T22:09:30Z" },
        "fable":   { "known": true, "used": 100, "reset_at": "2026-11-25T14:00:00Z", "observed_at": "2026-11-20T22:05:00Z" },
        "session": { "known": false, "used": 0 },     // 5-hour window; usually unknown on Max
        "backoff_until": "…",       // absent unless a claim failed in the last hour
        "grants": [
          { "id": "full-reset-2611", "label": "One full limit reset (November 2026)",
            "class": "weekly",      // weekly | fable | unsupported
            "clears_weekly": true, "clears_fable": true,
            "resets_total": 1, "resets_left": 1,
            "starts_at": "2026-11-10T21:30:00Z", "ends_at": "2026-12-10T22:00:00Z",
            "paused": false, "usable_now": true, "requires_limit": false, "blocked": false,
            "usable": true,         // CLIProxy could spend it now if a trigger fired
            "unusable_reason": ""   // account_ineligible | provider_cooldown | unsupported_grant | no_resets_left | paused |
                                    // not_usable_now | blocked | not_started | expired | requires_limit | pending |
                                    // unrecovered | backoff | settling
          }
        ]
      }
    },
    "operations": [                 // newest first, at most 50
      { "id": "…", "credential_id": "<raw>", "grant_id": "launch-2610-fable",
        "grant_label": "…", "class": "fable", "clears_fable": true, // clears_weekly / clears_fable: absent means false
        "trigger": "fable_need",    // weekly_need | fable_need | expiry
        "reason": "…", "resets_left_before": 1, "grant_ends_at": "…",
        "state": "done",            // pending | done | failed
        "result": "reset",          // reset | already_used | reconciled_reset | not_limited | cooldown | ineligible |
                                    // unavailable | rate_limited | auth_error | not_consumed | not_sent | unresolved |
                                    // identity_mismatch | identity_changed | account_removed
                                    // state "done" (reset, already_used, reconciled_reset) means a grant was spent;
                                    // unresolved may have spent one.
        "attempts": 1, "created_at": "…", "updated_at": "…", "confirmed_at": "…",
        "weekly_before": { "used": 100, "reset_at": "…" },   // cleared windows at claim time, when known
        "fable_before":  { "used": 100, "reset_at": "…" },
        "recovered": true }         // CLIProxy confirmed the reset from a fresh read and reopened routing
    ]
  }
}
```

### How CLIProxy decides (for wording and the "why" text)

- **Weekly need:** when the open weekly allowance across healthy, unheld
  accounts falls below 5 points and a request finally failed because of the
  weekly window within the last 15 minutes, it spends a weekly grant. An
  account whose 5-hour window stays full for more than 30 minutes counts as
  closed, but a fleet blocked only by 5-hour windows is not need, because no
  grant reopens them. It picks the exhausted account (≥ 95% used) whose weekly
  window renews last. One refused request justifies at most one reset.
- **Fable need:** the same check for Fable. It spends only Fable grants, on a
  Fable-exhausted account that has weekly allowance left right now.
- **Expiry:** in a grant's last 6 h, it spends the grant if the windows it
  clears hold at least 1% usage and none renews before the grant ends. An
  expiring grant costs nothing to use.
- **Safety rules:**
  - An account renewing within 30 min counts as open, and so does stale
    evidence.
  - An account whose last claim may have spent a reset counts as restored
    until a fresh read confirms it, so a reset is never spent twice on old
    data.
  - Need-triggered resets are at least 10 min apart and capped at 3 per day.
  - An unresolved claim quarantines only its own account, settles as
    `unresolved` after 1 h, and still counts against the cap.
  - A failed claim pauses that account for 1 h (`backoff_until`).
  - While routing is forced to one credential, the other accounts are shown
    with `healthy: false` and do not count as open allowance.

## Server-side changes

1. **Parsing (`internal/cliproxy`):** extend the raw `Accounts()` struct with
   `claude_resets`. Join `accounts` and `planned.account_id` by raw credential
   ID before hashing. Raw IDs, emails and labels that contain them must never
   reach the browser; follow the existing `credentialID`/`label` sanitisation.
   Grant labels are provider marketing text: keep them, but cap them at 120
   chars.
2. **Browser model:** add a normalised per-account `resets` object (count by
   class, next expiry, usable flag and reason, last automatic reset) and a fleet
   summary (mode/active, weekly and Fable trigger state and reason, total
   resets by class, earliest expiry). Mark the whole section `available:false`
   with a reason when CLIProxy does not report it.
3. **History (SQLite):** persist settled operations (`done`/`failed`), keyed by
   hashed account and operation ID, so the history survives CLIProxy journal
   trimming. Use a new migration. Retention must match existing history.
4. **Caching:** reuse the existing 60-second accounts cache and freshness rules.
   No extra upstream polling, and never call Anthropic directly.

## UI

Follow the existing helpers, tokens in `styles.css`, the quiet product tone,
keyboard focus and the mobile stacking rules. Add no dependencies or remote
assets.

1. **Claude pool card:** add a compact **Resets** row: weekly and Fable reset
   counts, earliest expiry ("next expires in 20 days"), and the automation state
   with its reason. When `weekly.state` is `spending` or an operation is
   `pending`, show it prominently. Do not add grants into the pool percentage
   or its forecasts. Show the extra capacity they represent separately: "+N
   weeks of allowance in reserve".
2. **Credential table:** add a Resets column (for example "1 weekly · 2 Fable")
   with the earliest expiry. Show a muted "not usable: <reason>" when no grant
   is usable.
3. **Per-account cards:** list each grant with its class badge, resets left,
   expiry countdown and usable state. Include the last automatic reset on that
   account. This app is read-only, so add no "reset now" button. A plain hint is
   enough: "Resets are spent automatically by CLIProxy".
4. **History:** add automatic resets as event markers on the existing Claude
   usage charts (a vertical marker with the grant class), plus a small table of
   the last operations.
5. **Off / inactive / absent:** `mode: off` shows grants muted with "Automatic
   resets off". `active:false` for other reasons shows "Automatic resets paused:
   <reason>". If the key is absent, hide the section with one short note.

## Tests and verification

- **Go unit tests:**
  - parser with the key present, absent, mode off, inactive, unsupported grant
    class, and missing times;
  - join by raw ID with no raw ID leaking into browser JSON;
  - operation persistence and retention.
- **JS tests:** extend `scripts/test-*.cjs` for expiry countdowns, labels and
  state reasons.
- **Browser smoke test:** extend the synthetic upstream in
  `scripts/verify-browser.cjs` with:
  - accounts holding weekly and Fable grants;
  - one account with no usable grant;
  - a pending operation;
  - an old-CLIProxy fixture without `claude_resets`.

  Check desktop and mobile, no horizontal overflow, keyboard focus, and that no
  secret or raw credential ID appears in browser responses.
- **Checks:** `go test ./...`, `go vet ./...`, `docker compose config --quiet`.
- **Docs:** update `README.md` (read-only contract and display semantics) and
  `PRODUCT.md`.

## Out of scope

- Changing CLIProxy, routing or settings.
- Spending or scheduling resets from the tracker.
- Adding upstream endpoints.
- Deploying without approval.

Production CLIProxy may still run a build without `claude_resets` until the
owner deploys it, so the absent case must look right.
