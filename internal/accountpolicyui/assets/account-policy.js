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
  const dashboard = window.PolicyDashboard;
  let telemetry = {},
    connectionEpoch = 0,
    accountPage = 1,
    settingsDirty = false,
    lastActivityStatus = "unavailable";
  let lastSnapshotAt = 0;
  const accountDrafts = new Map();
  function replaceMarkup(id, html) {
    const element = $(id);
    if (element.innerHTML === html) return;
    const focused = element.contains(document.activeElement)
      ? document.activeElement
      : null;
    const identity = focused?.dataset.focusKey || focused?.dataset.openAccount;
    element.innerHTML = html;
    if (focused) {
      const replacement = [
        ...element.querySelectorAll("[data-focus-key], [data-open-account]"),
      ].find(
        (el) => (el.dataset.focusKey || el.dataset.openAccount) === identity,
      );
      if (replacement) replacement.focus({ preventScroll: true });
      else {
        element.tabIndex = -1;
        element.focus({ preventScroll: true });
      }
    }
  }
  const API = "/v8/management/account-policy";
  let zone = "Europe/Berlin";
  const absolute = (value) => instant(value, zone),
    attr = (value) => escape(value),
    alias = (a) =>
      a.identity.alias || a.identity.account_id || a.identity.credential_id;
  const badge = (value, type = "") =>
    '<span class="badge ' + type + '">' + escape(value) + "</span>";
  $("origin").textContent = "Proxy: " + window.location.origin;
  function updateSettingHelp() {
    for (const id of ["automation", "fallback", "affinity"])
      $(id + "-help").textContent =
        dashboard.help[id][$(id).value] ||
        "Select an option to see its behavior.";
  }
  function filteredAccounts() {
    return dashboard.pageAccounts(state.accounts, {
      search: $("search").value,
      provider: $("provider-filter").value,
      status: $("status-filter").value,
      sort: $("sort").value,
      page: accountPage,
      pageSize: Number($("page-size").value),
    });
  }
  function changeView(view) {
    document.querySelectorAll("[data-section]").forEach((section) => {
      section.hidden = section.dataset.section !== view;
    });
    document.querySelectorAll("[data-view]").forEach((button) => {
      if (button.dataset.view === view)
        button.setAttribute("aria-current", "page");
      else button.removeAttribute("aria-current");
    });
  }
  function renderOverview() {
    const live = dashboard.activityView(
      telemetry.activity,
      telemetry.sampled_at,
      Date.now(),
    );
    lastActivityStatus = live.status;
    $("activity-badge").textContent =
      live.status === "live"
        ? live.requests
          ? "Live activity"
          : "Idle"
        : live.status === "stale"
          ? "Stale activity"
          : "Activity unavailable";
    $("activity-badge").className =
      "badge " + (live.status === "live" ? "good" : "warn");
    replaceMarkup(
      "serving",
      live.status !== "live"
        ? '<p class="muted">' +
            (live.status === "stale"
              ? "Last activity sample is stale. Current serving accounts are unknown until the next successful update."
              : "Current serving accounts are unavailable. Retained history is not live activity.") +
            "</p>"
        : live.active.length
          ? '<div class="serving-list">' +
            live.active
              .map(
                (a) =>
                  '<button type="button" class="serving-account" data-open-account="' +
                  attr(a.credential_id) +
                  '"><div><strong>' +
                  escape(a.alias || a.credential_id) +
                  "</strong><small>" +
                  escape(a.provider) +
                  " · " +
                  escape(a.credential_id) +
                  "</small></div><span>" +
                  escape(a.active_requests) +
                  " active request" +
                  (a.active_requests === 1 ? "" : "s") +
                  "</span></button>",
              )
              .join("") +
            "</div>"
          : '<p class="muted">No accounts are serving a request right now.</p>',
    );
    $("last-selected").textContent = live.latest
      ? "Last selected: " +
        (live.latest.alias || live.latest.credential_id) +
        " (" +
        live.latest.credential_id +
        ")" +
        " at " +
        absolute(live.latest.last_selected_at) +
        ". This is history, not a currently active account."
      : "No account selection recorded in this runtime yet.";
    $("sample-status").textContent = telemetry.sampled_at
      ? "Activity sampled " +
        absolute(telemetry.sampled_at) +
        "; updates every 5 seconds while visible"
      : "Activity unavailable; retrying while visible";
    const usage = telemetry.usage || {},
      totals = usage.totals || {};
    const collectionRequested =
      state.settings.enabled || state.settings.observations_enabled;
    const noCollection = collectionRequested ? "Unavailable" : "Collection off";
    const ratio =
      usage.available && totals.requests > 0
        ? ((100 * totals.success) / totals.requests).toFixed(1) + "%"
        : usage.available
          ? "No attempts yet"
          : noCollection;
    const quotas = state.accounts.map((a) =>
      dashboard.quotaView(a, state.settings, Date.now()),
    );
    const inventory = quotas.filter((q) => q.resets !== null);
    const usageMissing =
      usage.available && usage.collecting
        ? "Waiting for completions"
        : noCollection;
    const rows = [
      [
        "Recorded attempts",
        usage.available ? dashboard.number(totals.requests) : noCollection,
        "Usage records; retries and additional models may count separately",
      ],
      [
        "Measured tokens",
        totals.total_tokens != null
          ? dashboard.number(totals.total_tokens)
          : totals.requests > 0
            ? "Not measured"
            : usageMissing,
        totals.token_missing_requests > 0
          ? dashboard.number(totals.token_missing_requests) +
            " records lack token evidence; no total is inferred"
          : "Not an exact quota measurement",
      ],
      ["Success rate", ratio, "From recorded completions"],
      [
        "Average latency",
        usage.available && totals.average_latency_ms != null
          ? dashboard.number(totals.average_latency_ms) + " ms"
          : totals.requests > 0
            ? "Not measured"
            : usageMissing,
        "Completed request duration",
      ],
      [
        "Registered accounts",
        dashboard.number(state.accounts.length),
        "All pages and providers",
      ],
      [
        "Active requests",
        dashboard.number(live.requests),
        "Multiple accounts can serve concurrently",
      ],
      [
        inventory.some((q) => !q.inventoryFresh)
          ? "Last known saved resets"
          : "Available saved resets",
        inventory.length
          ? dashboard.number(inventory.reduce((sum, q) => sum + q.resets, 0))
          : "Unavailable",
        "Provider counts from " +
          inventory.length +
          " observed accounts; details may be incomplete",
      ],
    ];
    $("metrics").innerHTML = rows
      .map(
        ([label, value, help]) =>
          '<div class="metric"><span class="metric-label">' +
          escape(label) +
          '</span><strong class="metric-value' +
          (value === "Unavailable" ? " unavailable" : "") +
          '">' +
          escape(value) +
          "</strong><small>" +
          escape(help) +
          "</small></div>",
      )
      .join("");
    replaceMarkup(
      "usage-chart",
      dashboard.renderChart(usage, $("measure").value, absolute),
    );
    $("usage-chart").setAttribute("aria-busy", "false");
    $("coverage").textContent = usage.coverage_start
      ? "Retained observations: " +
        absolute(usage.coverage_start) +
        " to " +
        absolute(usage.coverage_end) +
        ". Gaps do not establish zero traffic. " +
        (usage.collecting
          ? "Collection is running."
          : "Collection is currently off; retained history remains visible.")
      : usage.available && usage.collecting
        ? "Collection is running. Waiting for real request completions; earlier traffic cannot be reconstructed."
        : collectionRequested
          ? "Usage data is unavailable; retrying. Check diagnostics for collection/storage errors."
          : "No retained usage observations. Enable Collect dashboard data in Routing & settings to record future completions without changing routing.";
    const points = dashboard.chartSeries(usage, $("measure").value);
    $("chart-values").innerHTML = points.length
      ? '<table><caption>Recorded values in the selected time zone</caption><thead><tr><th scope="col">Bucket starts</th><th scope="col">Requests</th><th scope="col">Tokens</th></tr></thead><tbody>' +
        points
          .map(
            (p) =>
              "<tr><td>" +
              escape(absolute(new Date(p.at).toISOString())) +
              "</td><td>" +
              escape(dashboard.number(p.requests)) +
              "</td><td>" +
              escape(dashboard.number(p.tokens)) +
              "</td></tr>",
          )
          .join("") +
        "</tbody></table>"
      : '<p class="muted">No measured values to display.</p>';
    const page = filteredAccounts();
    replaceMarkup(
      "allowances",
      dashboard.renderAllowances(
        page.items,
        state.settings,
        Date.now(),
        absolute,
      ),
    );
    $("allowance-scope").textContent =
      "Showing " +
      page.items.length +
      " of " +
      page.total +
      " matching accounts. Uses the Accounts filters and page.";
    replaceMarkup(
      "timeline",
      '<p class="field-help">Showing ' +
        page.items.length +
        " of " +
        page.total +
        " matching accounts, page " +
        page.page +
        " of " +
        page.pages +
        ". Search, filters and pages are in the Accounts view.</p>" +
        dashboard.renderTimeline(
          page.items,
          state.settings,
          Date.now(),
          Number($("timeline-range").value),
          absolute,
        ),
    );
    $("summary").textContent =
      state.accounts.length +
      " registered account" +
      (state.accounts.length === 1 ? "" : "s") +
      "; quota policy " +
      (state.settings.enabled ? "enabled" : "off");
  }
  function applyTelemetry(data) {
    telemetry = data || {};
    const live = dashboard.activityView(
      telemetry.activity,
      telemetry.sampled_at,
      Date.now(),
    );
    state.accounts = dashboard
      .mergeAccounts(state.observations || state.accounts, {
        ...telemetry.activity,
        available: live.status === "live",
      })
      .map((a) => ({
        ...a,
        control:
          (state.settings.accounts || {})[a.identity.credential_id] || {},
      }));
    renderOverview();
    // Never replace editable account forms while an operator is using them.
    if (
      !document.activeElement?.closest("#accounts") &&
      !$("accounts").querySelector(".account-details[open]")
    )
      renderAccountList();
    document.querySelectorAll("[data-runtime-count]").forEach((el) => {
      const account = state.accounts.find(
        (a) =>
          a.identity.credential_id ===
          el.closest("[data-account]").dataset.account,
      );
      if (account) el.textContent = dashboard.number(account.active_requests);
    });
    document.querySelectorAll("[data-runtime-status]").forEach((el) => {
      const account = state.accounts.find(
        (a) =>
          a.identity.credential_id ===
          el.closest("[data-account]").dataset.account,
      );
      if (!account) return;
      el.textContent =
        account.active_requests == null
          ? "Activity unavailable"
          : account.active_requests > 0
            ? "Serving now"
            : "Idle";
      el.className = "badge " + (account.active_requests > 0 ? "good" : "");
    });
  }
  const poller = dashboard.createPoller({
    isPaused: () =>
      !key ||
      busy ||
      document.hidden ||
      $("workspace").hidden ||
      $("confirm").open,
    fetchSnapshot: async () => {
      const data = await request("/dashboard?range=" + $("range").value);
      if (Date.now() - lastSnapshotAt >= 30000) {
        const [accounts, resets, decisions] = await Promise.all([
          request("/accounts"),
          request("/resets"),
          request("/decisions"),
        ]);
        data.snapshots = { accounts, resets, decisions };
      }
      return data;
    },
    applySnapshot: (data) => {
      if (data.snapshots) {
        state.observations = data.snapshots.accounts.accounts || [];
        state.settings = data.snapshots.accounts.settings || {};
        state.operations = data.snapshots.resets.operations || [];
        state.schedules = data.snapshots.resets.schedules || [];
        state.decisions = data.snapshots.decisions.decisions || [];
        lastSnapshotAt = Date.now();
        settingsForm();
        renderHistory();
      }
      applyTelemetry(data);
    },
    onError: () => {
      telemetry = {
        ...telemetry,
        activity: { ...telemetry.activity, available: false },
      };
      applyTelemetry(telemetry);
      $("sample-status").textContent =
        "Activity refresh failed; retrying. Usage values are retained observations.";
    },
  });
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
      if (key && !$("workspace").hidden) poller.refresh();
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
    const epoch = connectionEpoch;
    const [accounts, resets, decisions] = await Promise.all([
      request("/accounts"),
      request("/resets"),
      request("/decisions"),
    ]);
    let data;
    try {
      data = await request("/dashboard?range=" + $("range").value);
    } catch (_) {
      data = {
        activity: { available: false, accounts: [] },
        usage: { available: false },
      };
    }
    if (epoch !== connectionEpoch || !key) return false;
    state = {
      settings: accounts.settings || {},
      accounts: accounts.accounts || [],
      observations: accounts.accounts || [],
      operations: resets.operations || [],
      schedules: resets.schedules || [],
      decisions: decisions.decisions || [],
    };
    telemetry = data;
    state.accounts = dashboard
      .mergeAccounts(state.observations, {
        ...data.activity,
        available:
          dashboard.activityView(data.activity, data.sampled_at, Date.now())
            .status === "live",
      })
      .map((a) => ({
        ...a,
        control:
          (state.settings.accounts || {})[a.identity.credential_id] || {},
      }));
    lastSnapshotAt = Date.now();
    render();
    $("login").hidden = true;
    $("workspace").hidden = false;
    $("fixture-banner").hidden = !accounts.fixture_only;
    $("connection").textContent = accounts.fixture_only
      ? "Fixture preview"
      : "Connected";
    $("connection").className = "badge good";
    poller.start();
    return true;
  }
  function settingsForm() {
    if (settingsDirty) return;
    const s = state.settings;
    $("enabled").checked = !!s.enabled;
    $("observations-enabled").checked = !!s.observations_enabled;
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
      : s.observations_enabled
        ? "Dashboard collection only"
        : "Routing and collection off";
    $("policy-status").className = "badge " + (s.enabled ? "good" : "");
    updateSettingHelp();
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
    const activityBadge =
      '<span data-runtime-status class="badge ' +
      (a.active_requests > 0 ? "good" : "") +
      '">' +
      (a.active_requests == null
        ? "Activity unavailable"
        : a.active_requests > 0
          ? "Serving now"
          : "Idle") +
      "</span>";
    if (a.local_only)
      return (
        '<article class="account" data-account="' +
        attr(id) +
        '"><div class="account-header"><div><h3>' +
        escape(alias(a)) +
        '</h3><p class="account-identity">' +
        escape(a.identity.provider) +
        " · " +
        escape(id) +
        "</p></div>" +
        activityBadge +
        '</div><div class="account-meta">Active requests <span data-runtime-count>' +
        escape(dashboard.number(a.active_requests)) +
        '</span></div><p class="field-help">Quota, plan and reset expiry are unavailable until supported provider observations are collected. Local activity is independent of deadline routing.</p></article>'
      );
    const quota = dashboard.quotaView(a, state.settings, now);
    const quotaSummary =
      '<div class="account-quota-summary"><span>' +
      (quota.fresh ? "Weekly allowance left" : "Last known weekly allowance") +
      " <strong>" +
      (quota.remaining !== null
        ? escape(dashboard.number(quota.remaining)) + "%"
        : "Unavailable") +
      "</strong></span><span>Normal weekly refresh <strong>" +
      escape(absolute(quota.resetAt)) +
      "</strong></span><span>" +
      (quota.inventoryFresh ? "Saved resets" : "Last known saved resets") +
      " <strong>" +
      escape(dashboard.number(quota.resets)) +
      "</strong></span></div>";
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
      activityBadge +
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
      escape(dashboard.number(a.active_bindings)) +
      "</span><span>Active requests <span data-runtime-count>" +
      escape(dashboard.number(a.active_requests)) +
      "</span>" +
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
      "</span></div>" +
      quotaSummary +
      '<p class="time">Last successful usage refresh: ' +
      escape(absolute(a.observed_at)) +
      "<br>Inventory refresh: " +
      escape(absolute(a.inventory_observed_at)) +
      " · " +
      (a.inventory_complete
        ? "Complete"
        : "Incomplete: some credit details unavailable") +
      '</p><details class="account-details"><summary>Quota, saved resets &amp; account controls</summary>' +
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
              (state.settings.automation !== "auto_expiring" ||
                stamp(c.expires_at) <
                  Math.min(
                    ...(a.buckets || [])
                      .filter(
                        (b) =>
                          b.duration_seconds === 604800 &&
                          !b.model &&
                          (!b.scope || b.scope === "ordinary"),
                      )
                      .map((b) => stamp(b.reset_at) || Infinity),
                    Infinity,
                  )) &&
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
              ? "Expiry safety fallback: " +
                escape(
                  absolute(
                    new Date(
                      stamp(c.expires_at) -
                        (state.settings.expiry_guard_seconds || 0) * 1000,
                    ).toISOString(),
                  ),
                ) +
                ". Exhaustion can trigger earlier; backend eligibility remains authoritative."
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
      '> Hold this account<span class="field-help">Exclude from new deadline-policy routing, without interrupting active requests.</span></label><label>Allowance reserve (%)<input class="reserve" type="number" min="0" max="100" step="0.1" value="' +
      attr(controls.reserve_percent || 0) +
      '" required><span class="field-help">Percentage to keep unused. Not the number of saved reset credits.</span></label><button class="secondary" type="submit">Save account controls</button><button type="button" class="quiet" data-action="cooldown">Clear local cooldown</button></form></details></article>'
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
  function renderAccountList() {
    for (const article of $("accounts").querySelectorAll("[data-account]")) {
      const draft = accountDrafts.get(article.dataset.account);
      if (draft) {
        draft.open = !!article.querySelector(".account-details")?.open;
        draft.scheduleHidden =
          !!article.querySelector(".schedule-form")?.hidden;
      }
    }
    const provider = $("provider-filter").value;
    const providers = [
      ...new Set(
        state.accounts.map((a) => a.identity.provider).filter(Boolean),
      ),
    ].sort();
    $("provider-filter").innerHTML =
      '<option value="">All providers</option>' +
      providers
        .map(
          (p) => '<option value="' + attr(p) + '">' + escape(p) + "</option>",
        )
        .join("");
    if (providers.includes(provider)) $("provider-filter").value = provider;
    const page = filteredAccounts(),
      accounts = page.items;
    accountPage = page.page;
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
      '<div class="panel empty-state"><strong>No matching accounts</strong><p>Clear the filters or register credentials in the main dashboard. Quota observation must be enabled explicitly.</p></div>';
    $("account-count").textContent =
      page.total + " matching / " + state.accounts.length + " registered";
    $("page-summary").textContent =
      "Page " +
      page.page +
      " of " +
      page.pages +
      "; " +
      page.total +
      " matching accounts";
    $("previous-page").disabled = page.page <= 1;
    $("next-page").disabled = page.page >= page.pages;
    for (const article of $("accounts").querySelectorAll("[data-account]")) {
      const draft = accountDrafts.get(article.dataset.account);
      if (!draft || !article.querySelector(".account-details")) continue;
      article.querySelector(".account-details").open = draft.open;
      if (draft.controls) {
        article.querySelector(".hold").checked = draft.controls.hold;
        article.querySelector(".reserve").value = draft.controls.reserve;
      }
      if (draft.schedule) {
        const form = article.querySelector(".schedule-form");
        form.hidden = draft.scheduleHidden;
        form.dataset.credit = draft.schedule.credit;
        form.querySelector(".schedule-at").value = draft.schedule.at;
      }
      const hint = document.createElement("p");
      hint.className = "field-help";
      hint.textContent = "Unsaved account changes";
      article.append(hint);
    }
  }
  function render() {
    settingsForm();
    renderAccountList();
    renderOverview();
    renderHistory();
    tick();
  }
  function renderHistory() {
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
  }
  function tick() {
    clearTimeout(timer);
    const now = Date.now();
    let next = 60000;
    if (key) {
      next = 5000;
      if (
        dashboard.activityView(telemetry.activity, telemetry.sampled_at, now)
          .status !== lastActivityStatus
      )
        applyTelemetry(telemetry);
    }
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
      if (el)
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
    connectionEpoch++;
    poller.stop();
    key = $("key").value;
    $("key").value = "";
    run(async () => {
      if (await load())
        notice(
          "Connected. Provider automation remains at the server setting shown below.",
        );
    });
  });
  $("disconnect").addEventListener("click", () => {
    connectionEpoch++;
    poller.stop();
    key = "";
    accountDrafts.clear();
    state = {
      settings: {},
      accounts: [],
      operations: [],
      schedules: [],
      decisions: [],
    };
    telemetry = {};
    settingsDirty = false;
    $("unsaved").hidden = true;
    clearTimeout(timer);
    $("workspace").hidden = true;
    $("login").hidden = false;
    for (const id of [
      "accounts",
      "serving",
      "metrics",
      "usage-chart",
      "chart-values",
      "timeline",
      "decisions",
      "schedules",
      "operations",
    ])
      $(id).replaceChildren();
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
  document.querySelector(".dashboard-nav").addEventListener("click", (e) => {
    const button = e.target.closest("[data-view]");
    if (button) changeView(button.dataset.view);
  });
  for (const id of ["serving", "allowances"])
    $(id).addEventListener("click", (e) => {
      const button = e.target.closest("[data-open-account]");
      if (!button) return;
      $("search").value = button.dataset.openAccount;
      $("provider-filter").value = "";
      $("status-filter").value = "";
      accountPage = 1;
      renderAccountList();
      changeView("accounts");
    });
  for (const id of [
    "search",
    "provider-filter",
    "status-filter",
    "sort",
    "page-size",
  ])
    $(id).addEventListener(id === "search" ? "input" : "change", () => {
      accountPage = 1;
      renderAccountList();
      renderOverview();
    });
  $("previous-page").addEventListener("click", () => {
    accountPage--;
    renderAccountList();
    renderOverview();
  });
  $("next-page").addEventListener("click", () => {
    accountPage++;
    renderAccountList();
    renderOverview();
  });
  $("measure").addEventListener("change", renderOverview);
  $("timeline-range").addEventListener("change", renderOverview);
  $("range").addEventListener("change", () => poller.refresh());
  $("theme").addEventListener("change", () => {
    if ($("theme").value === "system")
      document.documentElement.removeAttribute("data-theme");
    else document.documentElement.dataset.theme = $("theme").value;
  });
  $("settings-form").addEventListener("input", () => {
    settingsDirty = true;
    $("unsaved").hidden = false;
    updateSettingHelp();
  });
  $("settings-form").addEventListener("change", () => {
    settingsDirty = true;
    $("unsaved").hidden = false;
    updateSettingHelp();
  });
  $("settings-form").addEventListener("submit", (e) => {
    e.preventDefault();
    run(async () => {
      const patch = {
        enabled: $("enabled").checked,
        observations_enabled: $("observations-enabled").checked,
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
      settingsDirty = false;
      $("unsaved").hidden = true;
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
      clearAccountDraft(a.identity.credential_id, "schedule");
      return;
    }
    if (action === "schedule") {
      const form = article.querySelector(".schedule-form");
      form.hidden = false;
      form.dataset.credit = button.dataset.credit;
      const draft = accountDrafts.get(a.identity.credential_id) || {};
      draft.schedule = {
        at: form.querySelector("input").value,
        credit: button.dataset.credit,
      };
      draft.open = true;
      draft.scheduleHidden = false;
      accountDrafts.set(a.identity.credential_id, draft);
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
  for (const event of ["input", "change"])
    $("accounts").addEventListener(event, (e) => {
      const article = e.target.closest("[data-account]");
      if (!article) return;
      const draft = accountDrafts.get(article.dataset.account) || {};
      if (e.target.closest(".account-controls"))
        draft.controls = {
          hold: article.querySelector(".hold").checked,
          reserve: article.querySelector(".reserve").value,
        };
      if (e.target.closest(".schedule-form"))
        draft.schedule = {
          at: article.querySelector(".schedule-at").value,
          credit: article.querySelector(".schedule-form").dataset.credit,
        };
      draft.open = !!article.querySelector(".account-details")?.open;
      draft.scheduleHidden = !!article.querySelector(".schedule-form")?.hidden;
      accountDrafts.set(article.dataset.account, draft);
    });
  function clearAccountDraft(id, section) {
    const draft = accountDrafts.get(id);
    if (!draft) return;
    delete draft[section];
    if (!draft.controls && !draft.schedule) accountDrafts.delete(id);
  }
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
        clearAccountDraft(a.identity.credential_id, "schedule");
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
        clearAccountDraft(a.identity.credential_id, "controls");
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
    if (!document.hidden && key) {
      tick();
      poller.refresh();
    }
  });
  window.addEventListener("focus", () => {
    if (key) {
      tick();
      poller.refresh();
    }
  });
  window.addEventListener("pageshow", () => {
    if (key) {
      tick();
      poller.refresh();
    }
  });
})();
