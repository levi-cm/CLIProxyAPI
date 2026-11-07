const { test } = require("node:test");
const assert = require("node:assert/strict");
const dashboard = require("./assets/account-policy-dashboard.js");

test("credit settings fall back to documented defaults", () => {
  assert.deepEqual(dashboard.creditSettings({}), {
    enabled: false,
    floor_credits: 7500,
    margin_credits: 250,
    allowance_guard_hours: 36,
    credit_value_usd: 0.04,
    usd_to_eur: 0.87,
  });
  const custom = dashboard.creditSettings({
    credit_spending: { enabled: true, floor_credits: 9000, credit_value_usd: 0.05 },
  });
  assert.equal(custom.enabled, true);
  assert.equal(custom.floor_credits, 9000);
  assert.equal(custom.credit_value_usd, 0.05);
  assert.equal(custom.margin_credits, 250);
});

test("credit view separates spendable balance, reserve and estimated value", () => {
  const view = dashboard.creditView(
    {
      state: "credits",
      balance: 62454.42,
      spendable: 54954.42,
      reserved: 7500,
      burn_per_hour: 300,
      fresh: true,
      observed_at: "2026-11-07T12:00:00Z",
    },
    { credit_spending: { enabled: true } },
  );
  assert.equal(view.label, "Serving: credits");
  assert.equal(view.spendable, 54954.42);
  assert.equal(view.reserved, 7500);
  assert.ok(Math.abs(view.value.usd - 2498.1768) < 1e-6);
  assert.ok(Math.abs(view.value.eur - 2173.41) < 0.01);
  assert.match(view.value.text, /€2,173/);
  assert.match(view.reserveValue.text, /€261/);
  assert.ok(Math.abs(view.runwayHours - 183.18) < 0.01);
});

test("unknown balances never invent value or runway", () => {
  const view = dashboard.creditView(
    { state: "unknown", balance: null, burn_per_hour: null },
    {},
  );
  assert.equal(view.balance, null);
  assert.equal(view.spendable, null);
  assert.equal(view.value, null);
  assert.equal(view.runwayHours, null);
  assert.equal(view.label, "Balance unknown");
  assert.equal(dashboard.creditView(undefined, {}), null);
});

test("fleet totals add accounts and ignore missing balances", () => {
  const fleet = dashboard.fleetCredits(
    {
      a: { state: "credits", balance: 62454.42, spendable: 54954.42, reserved: 7500, burn_per_hour: 300 },
      b: { state: "allowance", balance: 62500, spendable: 55000, reserved: 7500 },
      c: { state: "unknown", balance: null },
    },
    { credit_spending: { enabled: true } },
  );
  assert.equal(fleet.accounts, 2);
  assert.equal(fleet.serving, 1);
  assert.equal(fleet.reserved, 15000);
  assert.ok(Math.abs(fleet.spendable - 109954.42) < 1e-6);
  assert.ok(Math.abs(fleet.runwayHours - 109954.42 / 300) < 1e-6);
  assert.match(fleet.reserveValue.text, /€522/);
  assert.equal(dashboard.fleetCredits({}, {}), null);
});

test("durations stay readable", () => {
  assert.equal(dashboard.duration(0.5), "30 min");
  assert.equal(dashboard.duration(12.34), "12.3 h");
  assert.equal(dashboard.duration(183.18), "7.6 days");
  assert.equal(dashboard.duration(null), "Unavailable");
});
