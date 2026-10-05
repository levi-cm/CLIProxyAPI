const { test } = require("node:test");
const assert = require("node:assert/strict");
const ui = require("./assets/account-policy-dashboard.js");
const now = Date.parse("2026-10-05T12:00:00Z");
const account = (used = 46) => ({
  identity: {
    credential_id: "reference-a",
    alias: "Account <A>",
    provider: "codex",
  },
  observed_at: new Date(now).toISOString(),
  buckets: [
    {
      scope: "ordinary",
      duration_seconds: 604800,
      used_percent: used,
      reset_at: "2026-10-09T12:00:00Z",
    },
    {
      scope: "ordinary",
      duration_seconds: 18000,
      used_percent: 8,
      reset_at: "2026-10-05T14:00:00Z",
    },
  ],
  credits: [
    {
      id: "later",
      details_known: true,
      status: "available",
      expires_at: "2026-10-26T12:00:00Z",
    },
  ],
});
test("fractional allowance is exact and small positive values never look exhausted", () => {
  for (const [used, left] of [
    [46.25, "53.75"],
    [99.96, "0.04"],
    [46.27, "53.73"],
    [0.000001, "99.999999"],
  ]) {
    const html = ui.renderTimeline([account(used)], {}, now, 7, (v) => v);
    assert.ok(
      html.includes(`${left}% left`),
      `Expected ${left}% left for ${used}% used`,
    );
  }
});
test("projected windows expose their full estimated dates to assistive technology", () => {
  const html = ui.renderTimeline([account()], {}, now, 7, (v) => v);
  const accessible = html.match(
    /class="timeline-projection"[^>]*aria-label="([^"]*)"/,
  )[1];
  assert.match(accessible, /2026-10-09T12:00:00\.000Z/);
  assert.match(accessible, /2026-10-16T12:00:00\.000Z/);
  assert.match(accessible, /not.*observ/i);
});
test("calendar timeline uses fourteen local day boundaries, including DST", () => {
  const view = ui.timelineRange(now, "weekly", 0, "UTC");
  assert.equal(view.start, Date.parse("2026-09-28T00:00:00Z"));
  assert.equal(view.end, Date.parse("2026-10-12T00:00:00Z"));
  assert.equal(view.ticks.length, 14);
  const berlin = ui.timelineRange(
    Date.parse("2026-10-25T12:00:00Z"),
    "weekly",
    0,
    "Europe/Berlin",
  );
  assert.equal(berlin.end - berlin.start, 337 * 3600000);
  assert.equal(berlin.ticks[7].end - berlin.ticks[7].at, 25 * 3600000);
  assert.equal(ui.timelineRange(now, "weekly", 1, "UTC").start, view.end);
  assert.equal(ui.timelineRange(now, "weekly", -1, "UTC").end, view.start);
});
test("five-hour mode zooms to hours and uses the actual ordinary short window", () => {
  const view = ui.timelineRange(now, "5h", 0, "UTC");
  assert.equal(view.end - view.start, 12 * 3600000);
  assert.equal(view.ticks.length, 12);
  const html = ui.renderTimeline([account()], {}, now, 7, (v) => v, {
    mode: "5h",
    zone: "UTC",
  });
  assert.match(html, /92% left/);
  assert.match(html, /Five-hour window/);
  assert.match(html, /5h/);
  assert.doesNotMatch(html, /54% left/);
});
test("window geometry is independent of quota and exposes exact bounds and clipped endpoints", () => {
  const data = account();
  const start = now - 86400000,
    end = now + 86400000;
  const cycle = ui.quotaCycle(data.buckets[0], now, start, end, 604800);
  assert.equal(cycle.start, Date.parse("2026-10-02T12:00:00Z"));
  assert.equal(cycle.visibleStart, start);
  assert.equal(cycle.visibleEnd, end);
  assert.equal(cycle.elapsedEnd, now);
  assert.equal(
    ui.quotaCycle(
      { ...data.buckets[0], scope: "code_review" },
      now,
      start,
      end,
      604800,
    ),
    null,
  );
  const first = ui.renderTimeline([account(46)], {}, now, 7, (v) => v, {
    zone: "UTC",
  });
  const second = ui.renderTimeline([account(92)], {}, now, 7, (v) => v, {
    zone: "UTC",
  });
  assert.equal(
    first.match(/<rect[^>]*class="weekly-cycle-remaining"[^>]*>/)[0],
    second.match(/<rect[^>]*class="weekly-cycle-remaining"[^>]*>/)[0],
  );
  assert.match(first, /54% left/);
  assert.match(second, /8% left/);
  assert.match(first, /class="timeline-day-date"/);
  assert.match(first, /class="timeline-cycle-label"/);
  assert.match(first, /Estimated next window/);
  assert.doesNotMatch(first, /26\/10|2026-10-26/);
  assert.match(first, /1 later expiry/);
  assert.match(first, /Account &lt;A&gt;/);
});
test("unknown quota never becomes zero and future observation never becomes current quota", () => {
  for (const used of [null, -1, 101, "46", NaN]) {
    const html = ui.renderTimeline([account(used)], {}, now, 7, (v) => v);
    assert.match(html, /Allowance unavailable/);
    assert.doesNotMatch(html, /% left/);
  }
  const data = account();
  data.observed_at = new Date(now + 60000).toISOString();
  assert.match(
    ui.renderTimeline([data], {}, now, 7, (v) => v),
    /Allowance unavailable/,
  );
  const old = account();
  old.observed_at = new Date(now - 600000).toISOString();
  assert.match(
    ui.renderTimeline([old], {}, now, 7, (v) => v),
    /54% left \(last known\)/,
  );
});
