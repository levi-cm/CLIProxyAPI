const { test } = require("node:test");
const assert = require("node:assert/strict");
const dashboard = require("./assets/account-policy-dashboard.js");
const now = Date.parse("2026-10-04T16:00:00Z");
const identity = (id) => ({
  credential_id: id,
  alias: `Account ${id}`,
  provider: "codex",
});

test("serving now reports every concurrent account and never mistakes last selection for activity", () => {
  const accounts = [
    {
      ...identity("a"),
      active_requests: 2,
      last_selected_at: "2026-10-04T15:59:58Z",
    },
    {
      ...identity("b"),
      active_requests: 1,
      last_selected_at: "2026-10-04T15:59:59Z",
    },
    {
      ...identity("c"),
      active_requests: 0,
      last_selected_at: "2026-10-04T16:00:00Z",
    },
  ];
  const live = dashboard.activityView(
    { available: true, accounts },
    "2026-10-04T16:00:00Z",
    now,
  );
  assert.deepEqual(
    live.active.map((a) => a.credential_id),
    ["a", "b"],
  );
  assert.equal(live.requests, 3);
  assert.equal(live.latest.credential_id, "c");
  assert.equal(live.status, "live");
  assert.equal(
    dashboard.activityView(
      { available: true, accounts },
      "2026-10-04T15:59:30Z",
      now,
    ).status,
    "stale",
  );
  assert.equal(
    dashboard.activityView({ available: false, accounts: [] }, null, now)
      .status,
    "unavailable",
  );
  assert.equal(
    dashboard.activityView(
      { available: true, accounts: [] },
      "2026-10-04T16:01:00Z",
      now,
    ).status,
    "stale",
  );
});

test("a larger pool remains searchable and pageable by stable credential identity", () => {
  const accounts = Array.from({ length: 57 }, (_, i) => ({
    identity: identity(String(i).padStart(2, "0")),
    active_requests: i % 3 === 0 ? 1 : 0,
  }));
  const result = dashboard.pageAccounts(
    accounts,
    { page: 6, pageSize: 10, sort: "alias" },
    now,
  );
  assert.equal(result.total, 57);
  assert.equal(result.pages, 6);
  assert.deepEqual(
    result.items.map((a) => a.identity.credential_id),
    ["50", "51", "52", "53", "54", "55", "56"],
  );
  const filtered = dashboard.pageAccounts(
    accounts,
    { search: "Account 5", page: 6, pageSize: 10 },
    now,
  );
  assert.equal(filtered.total, 7);
  assert.equal(filtered.page, 1);
  assert.equal(
    dashboard.pageAccounts(accounts, { status: "active" }, now).total,
    19,
  );
  assert.equal(
    dashboard.pageAccounts(accounts, { provider: "gemini" }, now).total,
    0,
  );
  assert.equal(accounts[0].identity.credential_id, "00");
});

test("local activity augments unknown quota without inventing zero allowance", () => {
  const merged = dashboard.mergeAccounts([], {
    available: true,
    accounts: [{ ...identity("a"), active_requests: 1 }],
  });
  assert.equal(merged[0].active_requests, 1);
  assert.deepEqual(merged[0].buckets, []);
  assert.equal(merged[0].observed_at, null);
  assert.equal(merged[0].identity.credential_id, "a");
  assert.equal(merged[0].local_only, true);
  assert.equal(
    dashboard.mergeAccounts(
      [{ identity: identity("a"), active_requests: 12, buckets: [] }],
      { available: false, accounts: [] },
    )[0].active_requests,
    null,
  );
});

test("reset timelines distinguish refresh, expiry and fallback without fabricating unknown credits", () => {
  const account = {
    identity: identity("a"),
    buckets: [
      {
        scope: "ordinary",
        duration_seconds: 604800,
        reset_at: "2026-10-07T16:00:00Z",
      },
    ],
    credits: [
      {
        id: "earliest",
        details_known: true,
        status: "available",
        type: "codex_rate_limits",
        scopes: ["ordinary"],
        expires_at: "2026-10-05T04:00:00Z",
      },
      {
        id: "saved",
        details_known: true,
        status: "available",
        type: "codex_rate_limits",
        scopes: ["ordinary"],
        expires_at: "2026-11-05T04:00:00Z",
      },
      {
        id: "unknown",
        details_known: false,
        status: "available",
        expires_at: null,
      },
    ],
  };
  const events = dashboard.timelineEvents(
    account,
    {
      enabled: true,
      automation: "auto_expiring",
      credit_types: ["codex_rate_limits"],
      expiry_guard_seconds: 600,
    },
    now,
  );
  assert.deepEqual(events.map((e) => e.kind).sort(), [
    "expiry",
    "expiry",
    "guard",
    "weekly",
  ]);
  assert.equal(
    events.find((e) => e.kind === "guard").at,
    "2026-10-05T03:50:00.000Z",
  );
  assert.equal(
    dashboard.eventPosition("2026-11-05T04:00:00Z", now, now + 604800000)
      .outside,
    "after",
  );
  assert.equal(
    dashboard
      .timelineEvents(account, { enabled: true, automation: "off" }, now)
      .filter((e) => e.kind === "guard").length,
    0,
  );
});

test("charts use measured timestamps and explicit missing state, not invented zero points", () => {
  const usage = {
    available: true,
    coverage_start: "2026-10-04T15:00:00Z",
    series: [
      { at: "2026-10-04T15:30:00Z", requests: 8, total_tokens: 1483 },
      { at: "2026-10-04T14:30:00Z", requests: 0 },
      { at: "invalid", requests: 6 },
      { at: "2026-10-04T15:00:00Z", requests: 2, total_tokens: 381 },
      { at: "2026-10-04T15:45:00Z", requests: null },
    ],
  };
  assert.deepEqual(
    dashboard.chartSeries(usage, "requests").map((p) => p.value),
    [2, 8],
  );
  assert.deepEqual(
    dashboard.chartSeries(usage, "total_tokens").map((p) => p.value),
    [381, 1483],
  );
  assert.deepEqual(
    dashboard.chartSeries({ ...usage, available: false }, "requests"),
    [],
  );
});

test("background refresh publishes neither late disconnected results nor overlapping fetches", async () => {
  let finish,
    calls = 0,
    applied = 0;
  const poller = dashboard.createPoller({
    fetchSnapshot: () => {
      calls++;
      return new Promise((resolve) => {
        finish = resolve;
      });
    },
    applySnapshot: () => {
      applied++;
    },
    setTimer: () => 1,
    clearTimer: () => {},
    isPaused: () => false,
  });
  poller.start();
  const pending = poller.refresh();
  await Promise.resolve();
  await poller.refresh();
  assert.equal(calls, 1);
  poller.stop();
  finish({ activity: {} });
  await pending;
  assert.equal(applied, 0);
});

test("hidden or busy pages pause background requests", async () => {
  let calls = 0;
  const poller = dashboard.createPoller({
    fetchSnapshot: async () => {
      calls++;
    },
    applySnapshot: () => {},
    isPaused: () => true,
    setTimer: () => 1,
    clearTimer: () => {},
  });
  poller.start();
  await poller.refresh();
  assert.equal(calls, 0);
  poller.stop();
});

test("a paused timer keeps checking until the page is ready again", async () => {
  let paused = true,
    callback,
    calls = 0;
  const poller = dashboard.createPoller({
    fetchSnapshot: async () => {
      calls++;
    },
    applySnapshot: () => {},
    isPaused: () => paused,
    setTimer: (fn) => {
      callback = fn;
      return 1;
    },
    clearTimer: () => {},
  });
  poller.start();
  const first = callback;
  callback = null;
  await first();
  assert.equal(calls, 0);
  assert.equal(typeof callback, "function");
  paused = false;
  await callback();
  assert.equal(calls, 1);
  poller.stop();
});

test("usage charts show only occupied buckets without joining missing observations", () => {
  const usage = {
    available: true,
    range_seconds: 3600,
    bucket_seconds: 60,
    series: [
      { at: "2026-10-04T15:00:00Z", requests: 2 },
      { at: "2026-10-04T15:30:00Z", requests: 8 },
    ],
  };
  const html = dashboard.renderChart(usage, "requests", (v) => v, now);
  assert.equal((html.match(/class="chart-bar"/g) || []).length, 2);
  assert.ok(!html.includes('class="chart-line"'));
  assert.ok(html.includes("2026-10-04T16:00:00.000Z"));
});

test("missing token measures do not claim the known request records are absent", () => {
  const usage = {
    available: true,
    series: [{ at: "2026-10-04T15:00:00Z", requests: 3, total_tokens: null }],
  };
  assert.match(
    dashboard.renderChart(usage, "total_tokens", (v) => v, now),
    /No measured token values/,
  );
});

test("timeline advice respects the configured reset class whitelist", () => {
  const account = {
    identity: identity("a"),
    buckets: [],
    credits: [
      {
        id: "x",
        type: "codex_rate_limits",
        scopes: ["ordinary"],
        status: "available",
        details_known: true,
        expires_at: "2026-10-05T04:00:00Z",
      },
    ],
  };
  assert.deepEqual(
    dashboard
      .timelineEvents(
        account,
        { enabled: true, automation: "auto_expiring", credit_types: [] },
        now,
      )
      .map((e) => e.kind),
    ["expiry"],
  );
});

test("timeline axis retains full clock minutes instead of truncating timestamp text", () => {
  const html = dashboard.renderTimeline(
    [],
    {},
    now,
    1,
    () => "05/10/2026, 18:48 GMT+2",
  );
  assert.match(html, />05\/10 18:48<\/text>/);
});
