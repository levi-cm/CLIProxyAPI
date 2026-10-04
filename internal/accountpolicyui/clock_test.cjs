const { test } = require("node:test");
const assert = require("node:assert/strict");
const ui = require("./assets/account-policy.js");

test("expiry states never invent countdowns for missing detail or null expiry", () => {
  const now = Date.parse("2026-10-04T16:00:00Z");
  assert.equal(
    ui.creditState({ details_known: false, expires_at: null }, now),
    "Expiry unknown",
  );
  assert.equal(
    ui.creditState({ details_known: true, expires_at: null }, now),
    "Does not expire",
  );
  assert.equal(
    ui.creditState(
      { details_known: true, expires_at: "2026-10-04T15:00:00Z" },
      now,
    ),
    "Expired",
  );
  assert.equal(ui.countdown("2026-10-04T16:00:45Z", now), "45s");
  assert.equal(ui.countdown("2026-10-04T19:14:00Z", now), "3h 14m");
});

test("Berlin renders offset at each provider instant while fixed GMT+2 preserves UTC", () => {
  assert.match(
    ui.instant("2026-10-05T04:18:00Z", "Europe/Berlin"),
    /06:18.*GMT\+2/,
  );
  assert.match(
    ui.instant("2026-10-22T20:27:00Z", "Europe/Berlin"),
    /22:27.*GMT\+2/,
  );
  assert.match(
    ui.instant("2026-10-29T17:48:00Z", "Europe/Berlin"),
    /18:48.*GMT\+1/,
  );
  assert.match(ui.instant("2026-10-29T17:48:00Z", "GMT+2"), /19:48.*GMT\+2/);
});

test("manual schedules require an explicit UTC offset or Z", () => {
  assert.throws(() => ui.scheduleInstant("2026-10-29T19:00:00"), /offset/);
  assert.equal(
    ui.scheduleInstant("2026-10-29T19:00:00+01:00"),
    "2026-10-29T18:00:00.000Z",
  );
});

test("priority display selects latest authoritative decision only for owning credential", () => {
  const account = { identity: { credential_id: "b" } };
  const decisions = [
    {
      credential_id: "b",
      at: "2026-10-04T16:00:00Z",
      reason: "latest backend decision",
      deadline: "2026-10-05T15:50:00Z",
    },
    { credential_id: "a", at: "2026-10-04T17:00:00Z", reason: "other account" },
    {
      credential_id: "b",
      at: "2026-10-04T15:00:00Z",
      reason: "old backend decision",
    },
  ];
  assert.equal(
    ui.decisionFor(account, decisions).reason,
    "latest backend decision",
  );
  assert.equal(ui.decisionFor(account, []), null);
});

test("selected opaque credit stays with its owner after displayed sorting", () => {
  const a = {
    credits: [{ id: "october-29" }, { id: "october-05" }, { id: "october-22" }],
  };
  assert.equal(ui.selectedCredit(a, "october-05").id, "october-05");
  assert.equal(
    ui.selectedCredit({ credits: [{ id: "other" }] }, "october-05"),
    undefined,
  );
});

test("redeemed credit stops advertising a remaining redeemable expiry", () => {
  assert.equal(
    ui.creditState(
      {
        status: "redeemed",
        details_known: true,
        expires_at: "2026-10-05T16:00:00Z",
      },
      Date.parse("2026-10-04T16:00:00Z"),
    ),
    "Redeemed",
  );
});

test("provider writes reject purchased or unknown coverage and unhealthy accounts", () => {
  const now = Date.parse("2026-10-04T16:00:00Z"),
    settings = {
      enabled: true,
      credit_types: ["codex_rate_limits"],
      freshness_seconds: 120,
    };
  const account = {
    status: "healthy",
    inventory_observed_at: "2026-10-04T16:00:00Z",
    active_requests: 0,
  };
  const credit = {
    id: "saved",
    status: "available",
    details_known: true,
    type: "codex_rate_limits",
    scopes: ["ordinary"],
    expires_at: null,
  };
  assert.equal(ui.writeBlock(account, credit, settings, now), "");
  assert.match(
    ui.writeBlock(account, { ...credit, type: "purchased" }, settings, now),
    /unsupported/i,
  );
  assert.match(
    ui.writeBlock(account, { ...credit, scopes: [] }, settings, now),
    /coverage/i,
  );
  assert.match(
    ui.writeBlock({ ...account, status: "auth_failed" }, credit, settings, now),
    /authentication/i,
  );
  assert.match(
    ui.writeBlock(account, credit, { ...settings, enabled: false }, now),
    /enable/i,
  );
});
