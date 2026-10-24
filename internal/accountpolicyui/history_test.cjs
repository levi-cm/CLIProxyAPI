const test = require("node:test");
const assert = require("node:assert/strict");
const h = require("./assets/account-policy-history.js");
test("Berlin day rows retain actual 23 and 25 hour boundaries and repeated-hour offsets", () => {
  const spring = h.days(Date.parse("2026-03-29T12:00:00Z"), 0, "Europe/Berlin");
  const fall = h.days(Date.parse("2026-10-25T12:00:00Z"), 0, "Europe/Berlin");
  assert.equal(spring.at(-1).end - spring.at(-1).start, 23 * 3600000);
  assert.equal(fall.at(-1).end - fall.at(-1).start, 25 * 3600000);
  const svg = h.render([], fall, "Europe/Berlin");
  assert.match(svg, /02:00 GMT\+2/);
  assert.match(svg, /02:00 GMT\+1/);
  assert.equal(
    h.days(Date.parse("2026-10-25T12:00:00Z"), -150, "Europe/Berlin").length,
    7,
  );
});
test("traces preserve gaps reset steps midnight and avoid unobserved future lines", () => {
  const rows = h.days(Date.parse("2026-10-22T12:00:00Z"), 0, "UTC");
  const start = rows.at(-1).start;
  const points = [
    {
      observed_at: new Date(start + 3600000).toISOString(),
      remaining_percent: 20,
      event: "observation",
    },
    {
      observed_at: new Date(start + 3660000).toISOString(),
      remaining_percent: 95,
      event: "manual_reset",
    },
    {
      observed_at: new Date(start + 7200000).toISOString(),
      remaining_percent: 90,
      event: "observation",
    },
  ];
  const html = h.render(
    [{ id: "a", name: "<account>", observations: points }],
    rows,
    "UTC",
  );
  assert.match(html, /manual_reset/);
  assert.match(html, /&lt;account&gt;/);
  assert.match(html, /90%/);
  assert.equal((html.match(/<path /g) || []).length, 2);
  assert.ok(!html.includes("NaN"));
});

function fakeController(request) {
  const elements = new Map();
  const element = (id) => {
    if (!elements.has(id))
      elements.set(id, {
        value: "",
        innerHTML: "",
        textContent: "",
        disabled: false,
        listeners: {},
        addEventListener(name, fn) {
          this.listeners[name] = fn;
        },
        setAttribute() {},
        replaceChildren() {
          this.innerHTML = "";
        },
      });
    return elements.get(id);
  };
  global.document = { hidden: false, getElementById: element };
  const c = h.createController({
    request,
    accounts: () => [],
    filtered: () => [],
    alias: () => "",
    zone: () => "UTC",
    instant: (x) => x,
    countdown: () => "remaining",
    notice() {},
    download: async () => {},
  });
  return { c, element };
}
const emptyResponse = (path) =>
  path.startsWith("/usage-history/requests")
    ? { records: [] }
    : path.startsWith("/usage-history/summary")
      ? { groups: [] }
      : { enabled: true, healthy: true };
test("pressure routing sends account membership without percentages", async () => {
  let sent;
  const { c, element } = fakeController(async (path, method, body) => {
    if (path === "/routing-override" && method === "PUT") {
      sent = body;
      return { revision: 1, state: "active", mode: "pressure", accounts: body.accounts, expires_at: "2026-10-25T00:00:00Z" };
    }
    if (path === "/routing-override") return { revision: 0, state: "inactive" };
    return emptyResponse(path);
  });
  await c.refresh();
  element("override-mode").value = "pressure";
  element("override-account").value = "a";
  element("override-account-secondary").value = "b";
  element("override-hours").value = "1";
  await element("override-apply").listeners.click();
  assert.deepEqual(sent, { mode: "pressure", accounts: [{ credential_id: "a" }, { credential_id: "b" }], duration_hours: 1, expected_revision: 0 });
  assert.match(element("override-status").textContent, /a.*b.*concurrent pressure/);
  assert.doesNotMatch(element("override-status").textContent, /%/);
  c.reset();
  delete global.document;
});
test("history health exposes retained collection coverage gaps", async () => {
  const { c, element } = fakeController(async (path) => {
    if (path === "/routing-override") return { revision: 0, state: "inactive" };
    if (path === "/usage-history/status")
      return { enabled: true, healthy: true, uncertain_coverage: [{ from: "2026-10-22T10:00:00Z", to: "2026-10-22T11:00:00Z", reason: "collection_disabled" }] };
    return emptyResponse(path);
  });
  await c.refresh();
  assert.match(element("history-health").textContent, /1 coverage gap/);
  c.reset();
  delete global.document;
});
test("older override read cannot overwrite successful mutation", async () => {
  let count = 0,
    resolveOld;
  const { c, element } = fakeController(async (path, method) => {
    if (path === "/routing-override" && method === "PUT")
      return {
        revision: 1,
        state: "active",
        credential_id: "a",
        expires_at: "2026-10-23T00:00:00Z",
      };
    if (path === "/routing-override") {
      if (count++ === 0) return { revision: 0, state: "inactive" };
      return new Promise((r) => {
        resolveOld = r;
      });
    }
    return emptyResponse(path);
  });
  await c.refresh();
  const pending = c.refresh();
  await element("override-apply").listeners.click();
  resolveOld({ revision: 0, state: "inactive" });
  await pending;
  assert.match(element("override-status").textContent, /expires/);
  delete global.document;
});
test("summary group changes reject delayed older response", async () => {
  let oldResolve;
  const { c, element } = fakeController(async (path) => {
    if (path === "/routing-override") return { revision: 0, state: "inactive" };
    if (path.includes("/summary?") && path.includes("group=day"))
      return new Promise((r) => {
        oldResolve = r;
      });
    if (path.includes("/summary?"))
      return { groups: [{ period: "LATEST", model: "m", provider: "p" }] };
    return emptyResponse(path);
  });
  element("history-group").value = "day";
  const old = element("history-group").listeners.change();
  element("history-group").value = "month";
  await element("history-group").listeners.change();
  oldResolve({ groups: [{ period: "STALE", model: "m", provider: "p" }] });
  await old;
  assert.match(element("history-summary").innerHTML, /LATEST/);
  assert.doesNotMatch(element("history-summary").innerHTML, /STALE/);
  c.reset();
  delete global.document;
});

test("legacy overrides show current pressure semantics and remain cancellable", async () => {
  const { c, element } = fakeController(async (path, method) => {
    if (path.startsWith("/routing-override") && method === "DELETE") return { revision: 2, state: "cancelled" };
    if (path === "/routing-override") return { revision: 1, state: "active", mode: "split", accounts: [{ credential_id: "a", percent: 37 }, { credential_id: "b", percent: 63 }], expires_at: "2026-10-25T00:00:00Z" };
    return emptyResponse(path);
  });
  await c.refresh();
  assert.match(element("override-status").textContent, /a.*b.*concurrent pressure/);
  assert.doesNotMatch(element("override-status").textContent, /%/);
  assert.equal(element("override-cancel").disabled, false);
  await element("override-cancel").listeners.click();
  assert.match(element("override-status").textContent, /cancelled/);
  c.reset();
  delete global.document;
});
