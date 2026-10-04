/* Maintained policy extension. All business reads and writes use authenticated v8 APIs. */
(function () {
  "use strict";
  const stamp = (value) => {
    const n = Date.parse(value);
    return Number.isFinite(n) && n > 0 ? n : null;
  };
  function instant(value, zone) {
    const n = stamp(value);
    if (n === null) return "Unknown";
    if (zone === "GMT+2")
      return (
        new Intl.DateTimeFormat("en-GB", {
          timeZone: "UTC",
          year: "numeric",
          month: "2-digit",
          day: "2-digit",
          hour: "2-digit",
          minute: "2-digit",
          hourCycle: "h23",
        }).format(new Date(n + 7200000)) + " GMT+2 (fixed)"
      );
    try {
      return new Intl.DateTimeFormat("en-GB", {
        timeZone: zone || "Europe/Berlin",
        year: "numeric",
        month: "2-digit",
        day: "2-digit",
        hour: "2-digit",
        minute: "2-digit",
        hourCycle: "h23",
        timeZoneName: "shortOffset",
      }).format(new Date(n));
    } catch (_) {
      return new Date(n).toISOString() + " UTC";
    }
  }
  function countdown(value, now) {
    const n = stamp(value);
    if (n === null) return "Unknown";
    const s = Math.ceil((n - now) / 1000);
    if (s <= 0) return "Expired";
    if (s < 60) return s + "s";
    const m = Math.ceil(s / 60),
      h = Math.floor(m / 60),
      d = Math.floor(h / 24);
    if (d) return d + "d " + (h % 24) + "h " + (m % 60) + "m";
    if (h) return h + "h " + (m % 60) + "m";
    return m + "m";
  }
  function creditState(credit, now) {
    if (["redeemed", "consumed", "used"].includes(credit.status))
      return "Redeemed";
    if (!credit.details_known) return "Expiry unknown";
    if (credit.expires_at == null) return "Does not expire";
    const n = stamp(credit.expires_at);
    if (n === null) return "Expiry unknown";
    if (n <= now || credit.status === "expired") return "Expired";
    return "Expires in " + countdown(credit.expires_at, now);
  }
  function scheduleInstant(value) {
    if (!/T\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?(?:Z|[+-]\d{2}:\d{2})$/i.test(value))
      throw Error(
        "Enter an ISO date and time with an explicit UTC offset or Z.",
      );
    const n = stamp(value);
    if (n === null) throw Error("Invalid schedule timestamp.");
    return new Date(n).toISOString();
  }
  const fresh = (value, settings, now) =>
    stamp(value) !== null &&
    now - stamp(value) <= (settings.freshness_seconds || 120) * 1000 &&
    stamp(value) <= now + 5000;
  function decisionFor(account, decisions) {
    return (
      decisions
        .filter((d) => d.credential_id === account.identity.credential_id)
        .sort((a, b) => (stamp(b.at) || 0) - (stamp(a.at) || 0))[0] || null
    );
  }
  function selectedCredit(account, id) {
    return (account.credits || []).find((c) => c.id === id);
  }
  function writeBlock(account, credit, settings, now) {
    if (!settings.enabled)
      return "Enable policy to use selected reset controls.";
    if (settings.read_only || account.writes_disabled)
      return "Provider writes disabled.";
    if (
      !["healthy", "ready", "quota", "quota_blocked", "cooldown"].includes(
        account.status,
      )
    )
      return "Account authentication or health is unavailable.";
    if (!credit || !credit.id || !credit.details_known)
      return "Credit details or selected ID unavailable.";
    if (
      credit.type !== "codex_rate_limits" ||
      !(settings.credit_types || []).includes(credit.type)
    )
      return "Unsupported or unauthorized credit class.";
    if (!(credit.scopes || []).includes("ordinary"))
      return "Covered allowance unknown; ordinary coverage required.";
    if (credit.status !== "available")
      return "Credit is " + (credit.status || "unavailable") + ".";
    if (
      credit.expires_at != null &&
      (stamp(credit.expires_at) === null || stamp(credit.expires_at) <= now)
    )
      return "Credit expired or expiry unavailable.";
    if (!fresh(account.inventory_observed_at, settings, now))
      return "Inventory stale; refresh observations before using a reset.";
    if (account.active_requests > 0)
      return "Waiting for active inference to complete.";
    return "";
  }
  const helpers = {
    instant,
    countdown,
    creditState,
    scheduleInstant,
    decisionFor,
    selectedCredit,
    writeBlock,
  };
  if (typeof module !== "undefined" && module.exports) {
    module.exports = helpers;
    return;
  }
  const $ = (id) => document.getElementById(id),
    escape = (value) =>
      String(value ?? "").replace(
        /[&<>"']/g,
        (c) =>
          ({
            "&": "&amp;",
            "<": "&lt;",
            ">": "&gt;",
            '"': "&quot;",
            "'": "&#39;",
          })[c],
      );
  let key = "",
    state = {
      settings: {},
      accounts: [],
      operations: [],
      schedules: [],
      decisions: [],
    },
    timer,
    busy = false;
  const API = "/v8/management/account-policy";
  let zone = "Europe/Berlin";
  const absolute = (value) => instant(value, zone),
    attr = (value) => escape(value),
    alias = (a) =>
      a.identity.alias || a.identity.account_id || a.identity.credential_id;
  const badge = (value, type = "") =>
    '<span class="badge ' + type + '">' + escape(value) + "</span>";
  function notice(message, isError = false) {
    $("notice").textContent = message;
    $("notice").className = isError ? "error" : "";
    $("notice").hidden = false;
  }
  async function request(path, method = "GET", body, root = API) {
    if (!key) throw Error("Connect with your management key first.");
    const response = await fetch(root + path, {
      method,
      credentials: "omit",
      cache: "no-store",
      headers: {
        Authorization: "Bearer " + key,
        ...(body === undefined ? {} : { "Content-Type": "application/json" }),
      },
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
    });
    let data;
    try {
      data = await response.json();
    } catch (_) {
      throw Error("Unexpected management response (" + response.status + ").");
    }
    if (!response.ok) {
      const e = data.error;
      throw Error(
        (typeof e === "object"
          ? (e.code ? e.code + ": " : "") + e.message
          : e) || "Request failed (" + response.status + ").",
      );
    }
    return data;
  }
  async function run(action) {
    if (busy) return;
    busy = true;
    document.body.setAttribute("aria-busy", "true");
    try {
      await action();
    } catch (e) {
      notice(e.message, true);
    } finally {
      busy = false;
      document.body.removeAttribute("aria-busy");
    }
  }
  async function confirm(title, detail, actionLabel = "Confirm") {
    $("confirm-title").textContent = title;
    $("confirm-detail").textContent = detail;
    $("confirm-action").textContent = actionLabel;
    return await new Promise((resolve) => {
      const dialog = $("confirm");
      dialog.returnValue = "";
      dialog.addEventListener(
        "close",
        () => resolve(dialog.returnValue === "confirm"),
        { once: true },
      );
      dialog.showModal();
    });
  }
  async function load() {
    const [accounts, resets, decisions] = await Promise.all([
      request("/accounts"),
      request("/resets"),
      request("/decisions"),
    ]);
    state = {
      settings: accounts.settings || {},
      accounts: accounts.accounts || [],
      operations: resets.operations || [],
      schedules: resets.schedules || [],
      decisions: decisions.decisions || [],
    };
    render();
    $("login").hidden = true;
    $("workspace").hidden = false;
    $("fixture-banner").hidden = !accounts.fixture_only;
    $("connection").textContent = accounts.fixture_only
      ? "Fixture preview"
      : "Connected";
    $("connection").className = "badge good";
  }
  function settingsForm() {
    const s = state.settings;
    $("enabled").checked = !!s.enabled;
    $("automation").value = s.automation || "off";
    $("fallback").value = s.fallback || "round-robin";
    $("affinity").value = s.affinity || "strict";
    $("guard").value = s.expiry_guard_seconds ?? 600;
    $("saved-reserve").value = s.saved_credit_reserve ?? 1;
    $("read-only").checked = !!s.read_only;
    $("force").innerHTML =
      '<option value="">Automatic choice</option>' +
      state.accounts
        .map(
          (a) =>
            '<option value="' +
            attr(a.identity.credential_id) +
            '">' +
            escape(alias(a)) +
            "</option>",
        )
        .join("");
    $("force").value = s.force_account || "";
    $("policy-status").textContent = s.enabled
      ? "Enabled · " + s.automation
      : "Disabled";
    $("policy-status").className = "badge " + (s.enabled ? "good" : "");
  }
  function earliest(account) {
    return Math.min(
      ...(account.credits || [])
        .filter(
          (c) =>
            c.details_known &&
            c.status === "available" &&
            stamp(c.expires_at) > Date.now(),
        )
        .map((c) => stamp(c.expires_at)),
      Infinity,
    );
  }
  function allowed(a, c, now) {
    return writeBlock(a, c, state.settings, now) === "";
  }
  function renderAccount(a, first) {
    const now = Date.now(),
      id = a.identity.credential_id,
      controls = (state.settings.accounts || {})[id] || {},
      p = decisionFor(a, state.decisions),
      pending = state.operations.find(
        (o) =>
          o.credential_id === id &&
          [
            "planned",
            "prepared",
            "submitted",
            "verifying",
            "outcome_unknown",
          ].includes(o.state),
      );
    const credits = [...(a.credits || [])].sort(
      (x, y) =>
        (stamp(x.expires_at) || Infinity) - (stamp(y.expires_at) || Infinity),
    );
    return (
      '<article class="account ' +
      (first ? "earliest" : "") +
      '" data-account="' +
      attr(id) +
      '"><div class="account-header"><div><h3>' +
      escape(alias(a)) +
      '</h3><p class="account-identity">' +
      escape(a.identity.provider) +
      " · " +
      escape(a.plan || "Plan unknown") +
      "<br>Account " +
      escape(a.identity.account_id) +
      " · workspace " +
      escape(a.identity.workspace_id) +
      "<br>Credential " +
      escape(id) +
      " · generation " +
      escape(a.identity.generation) +
      '</p></div><div class="account-status">' +
      badge(a.status || "Unknown") +
      badge(
        a.eligible ? "Eligible" : "Ineligible",
        a.eligible ? "good" : "error",
      ) +
      (first ? badge("Earliest manual expiry", "warn") : "") +
      (controls.hold ? badge("On hold", "warn") : "") +
      (a.writes_disabled ? badge("Provider writes disabled", "error") : "") +
      (pending ? badge(pending.state, "warn") : "") +
      '</div></div><div class="account-meta"><span>Bindings ' +
      escape(a.active_bindings ?? 0) +
      "</span><span>Active requests " +
      escape(a.active_requests ?? 0) +
      "</span><span>Actual transport: " +
      escape(a.transport || "unavailable") +
      '</span><span data-age="' +
      attr(a.observed_at) +
      '">Snapshot age: ' +
      escape(countdownAge(a.observed_at, now)) +
      '</span><span class="inventory-status">' +
      badge(
        fresh(a.inventory_observed_at, state.settings, now)
          ? "Fresh inventory"
          : "Stale / unknown inventory",
        fresh(a.inventory_observed_at, state.settings, now) ? "good" : "warn",
      ) +
      '</span></div><p class="time">Last successful usage refresh: ' +
      escape(absolute(a.observed_at)) +
      "<br>Inventory refresh: " +
      escape(absolute(a.inventory_observed_at)) +
      " · " +
      (a.inventory_complete
        ? "Complete"
        : "Incomplete — some credit details unavailable") +
      "</p>" +
      (a.last_error
        ? '<p class="error time">Observation error: ' +
          escape(a.last_error) +
          "</p>"
        : "") +
      '<div class="advice">Latest backend routing decision: ' +
      escape(
        p ? p.reason : "No priority decision recorded for this account yet.",
      ) +
      (p && stamp(p.deadline) !== null
        ? "<br>Effective priority deadline at decision: " +
          escape(absolute(p.deadline))
        : "") +
      (p
        ? "<br>Model: " +
          escape(p.model) +
          " · Decision time: " +
          escape(absolute(p.at))
        : "") +
      "<br>Routing controls: " +
      escape(
        controls.hold
          ? "Held; excluded from new routing"
          : "Reserve " + (controls.reserve_percent || 0) + "%",
      ) +
      "<br>Requested model eligibility: " +
      escape(
        (a.buckets || [])
          .filter((b) => b.model)
          .map(
            (b) =>
              b.model + ": " + (b.allowed === false ? "blocked" : "observed"),
          )
          .join(", ") ||
          "See actual routing decisions; model restrictions apply.",
      ) +
      '</div><div class="buckets">' +
      (a.buckets || [])
        .map(
          (b) =>
            '<div class="bucket"><div class="bucket-head"><strong>' +
            escape(bucketLabel(b)) +
            "</strong><span>" +
            escape(b.used_percent) +
            '% used</span></div><meter min="0" max="100" value="' +
            attr(Math.max(0, Math.min(100, b.used_percent))) +
            '" aria-label="' +
            attr(bucketLabel(b) + " usage") +
            '"></meter><div class="time">' +
            escape(b.model || b.scope) +
            " · " +
            (b.allowed === false
              ? "Blocked"
              : "Allowance " + (b.allowed === true ? "available" : "unknown")) +
            "<br>" +
            escape(
              b.duration_seconds === 604800
                ? "Normal weekly reset"
                : "Normal window reset",
            ) +
            ": " +
            escape(absolute(b.reset_at)) +
            "</div></div>",
        )
        .join("") +
      '</div><div class="credit-list"><div class="section-heading"><h3>Manual reset expiry</h3><span class="muted">' +
      escape(a.available_credits) +
      " available · " +
      credits.length +
      " detailed rows</span></div>" +
      credits
        .map((c) => {
          const schedules = state.schedules.filter(
              (s) => s.credential_id === id && s.credit_id === c.id,
            ),
            auto =
              state.settings.enabled &&
              !state.settings.read_only &&
              !a.writes_disabled &&
              ["auto_expiring", "auto_all_saved"].includes(
                state.settings.automation,
              ) &&
              c.status === "available" &&
              c.details_known &&
              stamp(c.expires_at) > now &&
              (state.settings.credit_types || []).includes(c.type);
          return (
            '<div class="credit-row" data-credit-id="' +
            attr(c.id) +
            '"><div><div class="credit-title">' +
            escape(c.title || "Saved reset") +
            '</div><div class="credit-id">Credit ' +
            escape(c.id || "ID unavailable") +
            " · " +
            escape(c.type) +
            "<br>" +
            escape(c.status || "Status unknown") +
            " · Affects " +
            escape((c.scopes || []).join(", ") || "unknown allowance") +
            '<div class="time" data-credit-reason="' +
            attr(c.id) +
            '"></div>' +
            '</div></div><div><div class="time">Manual reset expires: ' +
            escape(
              c.details_known && c.expires_at == null
                ? "Does not expire"
                : absolute(c.expires_at),
            ) +
            '</div><div class="countdown" data-expiry="' +
            attr(c.expires_at || "") +
            '" data-known="' +
            !!c.details_known +
            '" data-status="' +
            attr(c.status) +
            '">' +
            escape(creditState(c, now)) +
            '</div><div class="time">' +
            (auto
              ? "Planned automatic redemption: " +
                escape(
                  absolute(
                    new Date(
                      stamp(c.expires_at) -
                        (state.settings.expiry_guard_seconds || 0) * 1000,
                    ).toISOString(),
                  ),
                ) +
                " · subject to fresh evidence and covered usage"
              : "Automatic redemption: " +
                escape(
                  state.settings.automation === "off"
                    ? "off"
                    : "not applicable",
                )) +
            "</div>" +
            schedules
              .map(
                (s) =>
                  '<div class="time">Scheduled redemption: ' +
                  escape(absolute(s.at)) +
                  "</div>",
              )
              .join("") +
            '</div><div class="credit-actions"><button data-action="redeem" data-credit="' +
            attr(c.id) +
            '" ' +
            (!allowed(a, c, now) || pending ? "disabled" : "") +
            '>Use reset</button><button class="secondary" data-action="schedule" data-credit="' +
            attr(c.id) +
            '" ' +
            (!allowed(a, c, now) || pending ? "disabled" : "") +
            ">Schedule</button></div></div>"
          );
        })
        .join("") +
      (a.available_credits >
      credits.filter((c) => c.status === "available").length
        ? '<p class="muted">Additional credit details unavailable. A count alone provides no credit IDs or expiry times.</p>'
        : "") +
      (!credits.length
        ? '<p class="muted">No detailed reset credits reported. Expiry unknown.</p>'
        : "") +
      '<form class="schedule-form" hidden><label>Local schedule · ISO time with offset<input class="schedule-at" required placeholder="2026-10-05T05:50:00+02:00" aria-label="Scheduled redemption time"></label><button type="submit">Save schedule</button><button type="button" class="secondary" data-action="hide-schedule">Cancel</button></form></div><form class="account-controls"><label class="check"><input class="hold" type="checkbox" ' +
      (controls.hold ? "checked" : "") +
      '> Hold this account</label><label>Allowance reserve (%)<input class="reserve" type="number" min="0" max="100" step="0.1" value="' +
      attr(controls.reserve_percent || 0) +
      '" required></label><button class="secondary" type="submit">Save account controls</button><button type="button" class="quiet" data-action="cooldown">Clear local cooldown</button></form></article>'
    );
  }
  function bucketLabel(b) {
    if (b.duration_seconds === 604800) return "Weekly allowance";
    if (b.duration_seconds === 18000) return "Short allowance · 5h";
    return "Allowance · " + b.duration_seconds / 3600 + "h";
  }
  function countdownAge(value, now) {
    const n = stamp(value);
    if (n === null) return "Unknown";
    const s = Math.max(0, Math.floor((now - n) / 1000));
    return s < 60 ? s + "s" : Math.floor(s / 60) + "m";
  }
  function render() {
    settingsForm();
    const accounts = [...state.accounts].sort((a, b) =>
      $("sort").value === "alias"
        ? alias(a).localeCompare(alias(b))
        : earliest(a) - earliest(b) || alias(a).localeCompare(alias(b)),
    );
    const min = Math.min(...accounts.map(earliest));
    const groups = new Map();
    for (const a of accounts) {
      const name = a.identity.provider || "Unknown provider";
      if (!groups.has(name)) groups.set(name, []);
      groups.get(name).push(a);
    }
    $("accounts").innerHTML =
      [...groups]
        .map(
          ([name, list]) =>
            '<h3 class="provider-heading">' +
            escape(name) +
            " · " +
            list.length +
            " account" +
            (list.length === 1 ? "" : "s") +
            "</h3>" +
            list
              .map((a) =>
                renderAccount(a, Number.isFinite(min) && earliest(a) === min),
              )
              .join(""),
        )
        .join("") ||
      '<p class="panel muted">No account snapshots yet. Enable observation in the server configuration and refresh.</p>';
    $("summary").textContent =
      accounts.length +
      " account" +
      (accounts.length === 1 ? "" : "s") +
      " · snapshot reads never poll the provider";
    $("schedules").innerHTML =
      state.schedules
        .map(
          (s) =>
            '<div class="history-row"><div><strong>Scheduled redemption: ' +
            escape(absolute(s.at)) +
            "</strong><br>Account " +
            escape(s.credential_id) +
            " · credit " +
            escape(s.credit_id) +
            '</div><button class="secondary" data-cancel="' +
            attr(s.id) +
            '">Cancel schedule</button></div>',
        )
        .join("") || '<p class="muted">No local schedules.</p>';
    $("operations").innerHTML =
      [...state.operations]
        .reverse()
        .map(
          (o) =>
            '<div class="history-row"><div><strong>' +
            escape(o.state) +
            "</strong> · account " +
            escape(o.account_id) +
            " · workspace " +
            escape(o.workspace_id) +
            "<br>Credit " +
            escape(o.credit_id) +
            " · result " +
            escape(o.result || "pending") +
            "<br>" +
            escape(o.error || "") +
            "</div><div>Updated " +
            escape(absolute(o.updated_at)) +
            "<br>Operation " +
            escape(o.id) +
            "<br>Stable request " +
            escape(o.request_id) +
            "</div></div>",
        )
        .join("") ||
      '<p class="muted">No redemption operations. Expired and unresolved operations remain here.</p>';
    $("decisions").innerHTML =
      [...state.decisions]
        .reverse()
        .slice(0, 30)
        .map(
          (d) =>
            '<div class="history-row"><div><strong>' +
            escape(d.credential_id || "Fallback") +
            "</strong> · " +
            escape(d.provider) +
            " / " +
            escape(d.model) +
            "<br>" +
            escape(d.reason) +
            (d.fallback ? " · fallback strategy used" : "") +
            "</div><div>Effective priority deadline: " +
            escape(absolute(d.deadline)) +
            "<br>Decision " +
            escape(absolute(d.at)) +
            "</div></div>",
        )
        .join("") ||
      '<p class="muted">No inference selection recorded yet.</p>';
    tick();
  }
  function tick() {
    clearTimeout(timer);
    const now = Date.now();
    let next = 60000;
    document.querySelectorAll("[data-expiry]").forEach((el) => {
      const c = {
        details_known: el.dataset.known === "true",
        expires_at: el.dataset.expiry || null,
        status: el.dataset.status,
      };
      el.textContent = creditState(c, now);
      const remaining =
        c.details_known && stamp(c.expires_at) !== null
          ? stamp(c.expires_at) - now
          : Infinity;
      el.className =
        "countdown" +
        (remaining <= 0 && stamp(c.expires_at) !== null
          ? " error"
          : remaining <= (state.settings.expiry_guard_seconds || 0) * 1000 &&
              remaining > 0
            ? " error"
            : remaining <= 3600000 && remaining > 0
              ? " warn"
              : remaining <= 86400000 && remaining > 0
                ? " warn"
                : "");
      if (remaining > 0 && remaining < 60000) next = 1000;
      else if (remaining >= 60000)
        next = Math.min(next, Math.max(1000, remaining - 59000));
      el.title =
        remaining > 0 &&
        remaining <= (state.settings.expiry_guard_seconds || 0) * 1000
          ? "Within expiry guard"
          : remaining > 0 && remaining <= 3600000
            ? "Expires within one hour"
            : remaining > 0 && remaining <= 86400000
              ? "Expires within 24 hours"
              : "";
      if (el.title && c.status === "available")
        el.textContent += " · " + el.title;
    });
    document.querySelectorAll("[data-age]").forEach((el) => {
      el.textContent = "Snapshot age: " + countdownAge(el.dataset.age, now);
    });
    document.querySelectorAll("[data-account]").forEach((article) => {
      const a = state.accounts.find(
        (a) => a.identity.credential_id === article.dataset.account,
      );
      if (!a) return;
      const pending = state.operations.some(
        (o) =>
          o.credential_id === a.identity.credential_id &&
          [
            "planned",
            "prepared",
            "submitted",
            "verifying",
            "outcome_unknown",
          ].includes(o.state),
      );
      article.querySelectorAll("[data-credit]").forEach((b) => {
        b.disabled =
          !selectedCredit(a, b.dataset.credit) ||
          !allowed(a, selectedCredit(a, b.dataset.credit), now) ||
          pending;
      });
      article.querySelectorAll("[data-credit-reason]").forEach((el) => {
        el.textContent = pending
          ? "An unresolved redemption blocks further resets."
          : writeBlock(
              a,
              selectedCredit(a, el.dataset.creditReason),
              state.settings,
              now,
            );
      });
      const el = article.querySelector(".inventory-status");
      el.innerHTML = badge(
        fresh(a.inventory_observed_at, state.settings, now)
          ? "Fresh inventory"
          : "Stale / unknown inventory",
        fresh(a.inventory_observed_at, state.settings, now) ? "good" : "warn",
      );
    });
    timer = setTimeout(tick, next);
  }
  $("login-form").addEventListener("submit", (e) => {
    e.preventDefault();
    key = $("key").value;
    $("key").value = "";
    run(async () => {
      await load();
      notice(
        "Connected. Provider automation remains at the server setting shown below.",
      );
    });
  });
  $("disconnect").addEventListener("click", () => {
    key = "";
    state = {
      settings: {},
      accounts: [],
      operations: [],
      schedules: [],
      decisions: [],
    };
    clearTimeout(timer);
    $("workspace").hidden = true;
    $("login").hidden = false;
    $("accounts").replaceChildren();
    $("connection").textContent = "Disconnected";
    $("connection").className = "badge";
    $("notice").hidden = true;
  });
  $("reload").addEventListener("click", () =>
    run(async () => {
      await load();
      notice("Snapshots reloaded. No provider refresh requested.");
    }),
  );
  $("refresh").addEventListener("click", () =>
    run(async () => {
      await request("/refresh", "POST", { credential_id: "" });
      await load();
      notice("Observations refreshed.");
    }),
  );
  $("export").addEventListener("click", () =>
    run(async () => {
      const data = await request("/diagnostics");
      const url = URL.createObjectURL(
        new Blob([JSON.stringify(data, null, 2)], { type: "application/json" }),
      );
      const link = document.createElement("a");
      link.href = url;
      link.download = "account-policy-diagnostics.json";
      link.click();
      setTimeout(() => URL.revokeObjectURL(url), 0);
      notice("Sanitized diagnostics exported.");
    }),
  );
  $("zone").addEventListener("change", () => {
    zone = $("zone").value;
    if (key) render();
  });
  $("sort").addEventListener("change", () => render());
  $("settings-form").addEventListener("submit", (e) => {
    e.preventDefault();
    run(async () => {
      const patch = {
        enabled: $("enabled").checked,
        automation: $("automation").value,
        fallback: $("fallback").value,
        affinity: $("affinity").value,
        expiry_guard_seconds: Number($("guard").value),
        saved_credit_reserve: Number($("saved-reserve").value),
        force_account: $("force").value,
        read_only: $("read-only").checked,
      };
      if (
        ["auto_expiring", "auto_all_saved"].includes(patch.automation) &&
        (!state.settings.enabled ||
          state.settings.automation !== patch.automation ||
          (state.settings.read_only && !patch.read_only))
      ) {
        if (
          !(await confirm(
            "Enable automatic provider redemption?",
            "Mode: " +
              patch.automation +
              "\nEligible saved credits may be redeemed on their owning accounts. Purchased resets are excluded. Known provider expiry remains unchanged.",
            "Save automation",
          ))
        )
          return;
      }
      await request("/settings", "PATCH", patch);
      await load();
      notice("Policy settings saved.");
    });
  });
  $("accounts").addEventListener("click", (e) => {
    const button = e.target.closest("button[data-action]");
    if (!button) return;
    const article = button.closest("[data-account]"),
      a = state.accounts.find(
        (a) => a.identity.credential_id === article.dataset.account,
      ),
      action = button.dataset.action,
      c = selectedCredit(a, button.dataset.credit);
    if (action === "hide-schedule") {
      article.querySelector(".schedule-form").hidden = true;
      return;
    }
    if (action === "schedule") {
      const form = article.querySelector(".schedule-form");
      form.hidden = false;
      form.dataset.credit = button.dataset.credit;
      form.querySelector("input").focus();
      return;
    }
    run(async () => {
      if (action === "redeem") {
        if (
          !(await confirm(
            "Use reset for " + alias(a) + "?",
            "Account: " +
              a.identity.account_id +
              "\nWorkspace: " +
              a.identity.workspace_id +
              "\nCredit: " +
              c.id +
              "\nAffected allowance: " +
              (c.scopes || []).join(", ") +
              "\nManual reset expiry: " +
              absolute(c.expires_at) +
              "\nNormal weekly reset is refreshed only after provider evidence confirms recovery.",
            "Use this reset",
          ))
        )
          return;
        let data;
        try {
          data = await request("/resets/redeem", "POST", {
            credential_id: a.identity.credential_id,
            credit_id: c.id,
          });
        } catch (error) {
          // A failed response may still have a durable operation to display.
          try {
            await load();
          } catch (_) {
            /* Preserve the original error. */
          }
          throw error;
        }
        await load();
        notice(
          "Redemption operation: " +
            data.operation.state +
            ". Inventory and quota now show backend evidence.",
        );
      }
      if (action === "cooldown") {
        if (
          !(await confirm(
            "Clear local cooldown for " + alias(a) + "?",
            "Account: " +
              a.identity.account_id +
              "\nThis clears proxy routing state only. It does not redeem a credit, refresh provider quota, or renew OAuth.",
            "Clear local cooldown",
          ))
        )
          return;
        const data = await request(
          "/credentials",
          "GET",
          undefined,
          "/v8/management",
        );
        const credential = (data.files || []).find(
          (f) => f.id === a.identity.credential_id,
        );
        if (!credential || !credential.auth_index)
          throw Error(
            "Credential index is unavailable; use the existing management panel cooldown control.",
          );
        await request(
          "/routing/cooldown/reset",
          "POST",
          { auth_index: credential.auth_index },
          "/v8/management",
        );
        await load();
        notice(
          "Local proxy cooldown cleared. Provider quota was not redeemed.",
        );
      }
    });
  });
  $("accounts").addEventListener("submit", (e) => {
    e.preventDefault();
    const form = e.target,
      article = form.closest("[data-account]"),
      a = state.accounts.find(
        (a) => a.identity.credential_id === article.dataset.account,
      );
    run(async () => {
      if (form.classList.contains("schedule-form")) {
        const c = selectedCredit(a, form.dataset.credit),
          at = scheduleInstant(form.querySelector("input").value);
        if (stamp(at) <= Date.now())
          throw Error("Schedule must be in the future.");
        if (stamp(c.expires_at) !== null && stamp(at) >= stamp(c.expires_at))
          throw Error(
            "Schedule must precede the selected credit provider expiry.",
          );
        if (
          !(await confirm(
            "Schedule reset for " + alias(a) + "?",
            "Account: " +
              a.identity.account_id +
              "\nWorkspace: " +
              a.identity.workspace_id +
              "\nCredit: " +
              c.id +
              "\nScheduled redemption: " +
              absolute(at) +
              "\nProvider expiry: " +
              (c.expires_at == null
                ? "Does not expire"
                : absolute(c.expires_at)),
            "Save this schedule",
          ))
        )
          return;
        await request("/resets/schedule", "POST", {
          credential_id: a.identity.credential_id,
          credit_id: c.id,
          at,
        });
        await load();
        notice(
          "Local redemption schedule saved. Provider expiry is unchanged.",
        );
      } else {
        const accounts = {
          ...(state.settings.accounts || {}),
          [a.identity.credential_id]: {
            hold: form.querySelector(".hold").checked,
            reserve_percent: Number(form.querySelector(".reserve").value),
          },
        };
        await request("/settings", "PATCH", { accounts });
        await load();
        notice("Account hold and reserve saved.");
      }
    });
  });
  $("schedules").addEventListener("click", (e) => {
    const b = e.target.closest("[data-cancel]");
    if (b)
      run(async () => {
        await request(
          "/resets/schedule/" + encodeURIComponent(b.dataset.cancel),
          "DELETE",
        );
        await load();
        notice("Local schedule cancelled.");
      });
  });
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden && key) tick();
  });
  window.addEventListener("focus", () => {
    if (key) tick();
  });
  window.addEventListener("pageshow", () => {
    if (key) tick();
  });
})();
