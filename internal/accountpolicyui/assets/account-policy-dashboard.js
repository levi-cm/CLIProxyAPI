/* Dashboard telemetry helpers. Browser reads remain authenticated and local to the proxy. */
(function (root) {
  "use strict";
  const stamp = (value) => {
    const at = Date.parse(value);
    return Number.isFinite(at) && at > 0 ? at : null;
  };
  const escape = (value) =>
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
  const name = (account) =>
    account.identity?.alias ||
    account.alias ||
    account.identity?.credential_id ||
    account.credential_id;
  const measured = (value) =>
    typeof value === "number" && Number.isFinite(value) && value >= 0;
  const number = (value) =>
    measured(value)
      ? new Intl.NumberFormat("en-GB", { maximumFractionDigits: 1 }).format(
          value,
        )
      : "Unavailable";
  // Display evidence may outlive the stricter backend action freshness window.
  function quotaView(account, settings, now) {
    const validTime = (value) => {
      const at = stamp(value);
      return at !== null && at <= now ? at : null;
    };
    const bucket = (account.buckets || []).find(
      (b) =>
        b.duration_seconds === 604800 &&
        !b.model &&
        (!b.scope || b.scope === "ordinary"),
    );
    const at = validTime(bucket?.observed_at || account.observed_at);
    const inventoryAt = validTime(account.inventory_observed_at);
    const limit = (settings.freshness_seconds || 120) * 1000;
    return {
      remaining:
        bucket &&
        at !== null &&
        measured(bucket.used_percent) &&
        bucket.used_percent <= 100
          ? 100 - bucket.used_percent
          : null,
      observedAt: at,
      fresh: at !== null && now - at <= limit,
      resetAt: bucket?.reset_at,
      resets:
        inventoryAt !== null && measured(account.available_credits)
          ? account.available_credits
          : null,
      inventoryFresh: inventoryAt !== null && now - inventoryAt <= limit,
    };
  }
  function activityView(activity, sampledAt, now) {
    const accounts = activity?.accounts || [];
    const latest =
      [...accounts]
        .filter((a) => stamp(a.last_selected_at) !== null)
        .sort(
          (a, b) => stamp(b.last_selected_at) - stamp(a.last_selected_at),
        )[0] || null;
    const at = stamp(sampledAt);
    const status = !activity?.available
      ? "unavailable"
      : at === null || now - at > 15000 || at > now + 5000
        ? "stale"
        : "live";
    const active =
      status === "live"
        ? accounts.filter(
            (a) => measured(a.active_requests) && a.active_requests > 0,
          )
        : [];
    return {
      status,
      active,
      requests:
        status === "live"
          ? active.reduce((n, a) => n + a.active_requests, 0)
          : null,
      latest,
    };
  }
  function mergeAccounts(snapshots, activity) {
    const runtime = new Map(
      (activity?.accounts || []).map((a) => [a.credential_id, a]),
    );
    const result = new Map(
      snapshots.map((a) => [
        a.identity.credential_id,
        { ...a, active_requests: null, active_bindings: null },
      ]),
    );
    for (const [id, live] of runtime) {
      const account = result.get(id) || {
        identity: {
          credential_id: id,
          alias: live.alias,
          provider: live.provider,
        },
        status: live.status || "unobserved",
        eligible: false,
        observed_at: null,
        buckets: [],
        credits: [],
        available_credits: null,
        local_only: true,
      };
      result.set(id, {
        ...account,
        active_requests: activity.available
          ? (live.active_requests ?? null)
          : null,
        active_bindings: activity.available
          ? (live.active_bindings ?? null)
          : null,
        transport: live.transport || account.transport,
        last_selected_at: live.last_selected_at,
        disabled: live.disabled,
      });
    }
    return [...result.values()];
  }
  function firstExpiry(account, now) {
    return Math.min(
      ...(account.credits || [])
        .filter(
          (c) =>
            c.details_known &&
            c.status === "available" &&
            stamp(c.expires_at) > now,
        )
        .map((c) => stamp(c.expires_at)),
      Infinity,
    );
  }
  function pageAccounts(accounts, options = {}, now = Date.now()) {
    const search = (options.search || "").trim().toLocaleLowerCase();
    const items = accounts
      .filter((a) => {
        const text = [
          name(a),
          a.identity.provider,
          a.identity.account_id,
          a.identity.workspace_id,
          a.identity.credential_id,
        ]
          .join(" ")
          .toLocaleLowerCase();
        return (
          (!search || text.includes(search)) &&
          (!options.provider || a.identity.provider === options.provider) &&
          (!options.status ||
            (options.status === "active" && a.active_requests > 0) ||
            (options.status === "held" && a.control?.hold) ||
            (options.status === "unknown" && !stamp(a.observed_at)))
        );
      })
      .sort((a, b) =>
        options.sort === "expiry"
          ? firstExpiry(a, now) - firstExpiry(b, now) ||
            String(name(a)).localeCompare(String(name(b)))
          : String(name(a)).localeCompare(String(name(b))),
      );
    const pageSize = Math.max(1, Math.min(100, Number(options.pageSize) || 10));
    const pages = Math.max(1, Math.ceil(items.length / pageSize));
    const page = Math.min(pages, Math.max(1, Number(options.page) || 1));
    return {
      items: items.slice((page - 1) * pageSize, page * pageSize),
      total: items.length,
      pages,
      page,
    };
  }
  function timelineEvents(account, settings, now) {
    const events = [];
    for (const bucket of account.buckets || []) {
      if (
        !stamp(bucket.reset_at) ||
        (bucket.scope && bucket.scope !== "ordinary") ||
        bucket.model
      )
        continue;
      events.push({
        kind: bucket.duration_seconds === 604800 ? "weekly" : "window",
        at: bucket.reset_at,
        label:
          bucket.duration_seconds === 604800
            ? "Normal weekly refresh"
            : "Normal short-window refresh",
      });
    }
    const weekly = Math.min(
      ...events
        .filter((e) => e.kind === "weekly" && stamp(e.at) > now)
        .map((e) => stamp(e.at)),
      Infinity,
    );
    for (const credit of account.credits || []) {
      if (
        !credit.details_known ||
        !stamp(credit.expires_at) ||
        !["available", "expired"].includes(credit.status)
      )
        continue;
      events.push({
        kind: "expiry",
        at: credit.expires_at,
        label: "Manual reset expires",
        credit: credit.id,
      });
      if (
        settings.enabled &&
        !settings.read_only &&
        !account.writes_disabled &&
        ["auto_expiring", "auto_all_saved"].includes(settings.automation) &&
        credit.status === "available" &&
        credit.type === "codex_rate_limits" &&
        settings.credit_types?.includes(credit.type) &&
        credit.scopes?.includes("ordinary") &&
        stamp(credit.expires_at) > now &&
        (settings.automation === "auto_all_saved" ||
          stamp(credit.expires_at) < weekly)
      ) {
        events.push({
          kind: "guard",
          at: new Date(
            stamp(credit.expires_at) -
              (settings.expiry_guard_seconds || 0) * 1000,
          ).toISOString(),
          label: "Expiry safety fallback; exhaustion can trigger earlier",
          credit: credit.id,
        });
      }
    }
    return events.sort((a, b) => stamp(a.at) - stamp(b.at));
  }
  function eventPosition(at, start, end) {
    const value = stamp(at);
    if (value === null || end <= start)
      return { position: null, outside: null };
    return {
      position: Math.max(
        0,
        Math.min(100, ((value - start) / (end - start)) * 100),
      ),
      outside: value < start ? "before" : value > end ? "after" : null,
    };
  }
  function chartSeries(usage, metric) {
    if (!usage?.available) return [];
    const coverage = stamp(usage.coverage_start);
    return (usage.series || [])
      .filter(
        (p) =>
          stamp(p.at) !== null &&
          measured(p[metric]) &&
          (!coverage ||
            stamp(p.at) + (usage.bucket_seconds || 0) * 1000 >= coverage),
      )
      .map((p) => ({
        at: stamp(p.at),
        value: p[metric],
        requests: p.requests,
        tokens: p.total_tokens,
      }))
      .sort((a, b) => a.at - b.at);
  }
  function createPoller(options) {
    const setTimer = options.setTimer || setTimeout,
      clearTimer = options.clearTimer || clearTimeout;
    let running = false,
      pending = false,
      timer,
      epoch = 0;
    function schedule() {
      clearTimer(timer);
      if (running) timer = setTimer(() => refresh(), 5000);
    }
    async function refresh() {
      if (!running || pending) return;
      if (options.isPaused?.()) {
        schedule();
        return;
      }
      clearTimer(timer);
      pending = true;
      const current = epoch;
      try {
        const data = await options.fetchSnapshot();
        if (running && current === epoch) options.applySnapshot(data);
      } catch (error) {
        if (running && current === epoch) options.onError?.(error);
      } finally {
        pending = false;
        schedule();
      }
    }
    return {
      start() {
        if (!running) {
          running = true;
          epoch++;
          schedule();
        }
      },
      stop() {
        running = false;
        epoch++;
        clearTimer(timer);
      },
      refresh,
    };
  }
  function renderChart(usage, metric, absolute, now = Date.now()) {
    const points = chartSeries(usage, metric);
    if (!points.length)
      return (
        '<div class="empty-state"><strong>' +
        (usage?.available
          ? metric === "total_tokens"
            ? "No measured token values available"
            : "No recorded attempts in this range"
          : "Usage history unavailable") +
        "</strong><p>History starts with retained proxy measurements. Tokens are not subscription allowance.</p></div>"
      );
    const width = 900,
      height = 230,
      left = 58,
      right = 14,
      top = 20,
      bottom = 40;
    const max = Math.max(1, ...points.map((p) => p.value));
    const end = now,
      start = end - (usage.range_seconds || 86400) * 1000;
    const bucket = (usage.bucket_seconds || 60) * 1000;
    const x = (p) =>
      left +
      (end === start ? 0.5 : (p.at - start) / (end - start)) *
        (width - left - right);
    const y = (p) => top + (1 - p.value / max) * (height - top - bottom);
    const barWidth = Math.max(
      2,
      Math.min(36, (bucket / (end - start)) * (width - left - right) * 0.8),
    );
    const grids = [0, 0.5, 1]
      .map((f) => {
        const yy = top + (1 - f) * (height - top - bottom);
        return `<line x1="${left}" y1="${yy}" x2="${width - right}" y2="${yy}" class="chart-grid"/><text x="${left - 10}" y="${yy + 4}" text-anchor="end">${escape(number(max * f))}</text>`;
      })
      .join("");
    return (
      `<svg class="usage-chart" viewBox="0 0 ${width} ${height}" role="img" aria-label="${metric === "requests" ? "Completed requests" : "Measured tokens"} per recorded bucket; missing observations are not plotted">${grids}` +
      points
        .filter((p) => p.at + bucket >= start && p.at <= end)
        .map(
          (p) =>
            `<rect tabindex="0" data-focus-key="${metric}:${p.at}" x="${Math.max(left, Math.min(width - right - barWidth, x({ at: p.at + bucket / 2 }) - barWidth / 2))}" y="${y(p)}" width="${barWidth}" height="${height - bottom - y(p)}" class="chart-bar"><title>${escape(absolute(new Date(p.at).toISOString()) + ": " + number(p.value) + (metric === "requests" ? " requests" : " tokens"))}</title></rect>`,
        )
        .join("") +
      `<text x="${left}" y="${height - 10}">${escape(absolute(new Date(start).toISOString()))}</text><text x="${width - right}" y="${height - 10}" text-anchor="end">${escape(absolute(new Date(end).toISOString()))}</text></svg>`
    );
  }
  function renderTimeline(accounts, settings, now, days, absolute) {
    const end = now + days * 86400000;
    const axis = [0, 0.25, 0.5, 0.75, 1]
      .map(
        (f) =>
          `<text x="${20 + f * 860}" y="20" text-anchor="${f === 0 ? "start" : f === 1 ? "end" : "middle"}">${escape(f === 0 ? "Now" : absolute(new Date(now + (end - now) * f).toISOString()).replace(/(\d{2}\/\d{2})\/\d{4}, (\d{2}:\d{2}).*/, "$1 $2"))}</text>`,
      )
      .join("");
    const rows = accounts
      .map((a) => {
        const events = timelineEvents(a, settings, now);
        const inside = events.filter(
          (e) => !eventPosition(e.at, now, end).outside,
        );
        const outside = events.filter(
          (e) => eventPosition(e.at, now, end).outside,
        );
        const marks = inside
          .map((e) => {
            const position = eventPosition(e.at, now, end).position;
            return `<g class="timeline-marker ${e.kind}" tabindex="0" data-focus-key="${escape(a.identity.credential_id + ":" + e.kind + ":" + (e.credit || "") + ":" + e.at)}" role="img" aria-label="${escape(e.label + ": " + absolute(e.at))}"><title>${escape(e.label + ": " + absolute(e.at) + (e.credit ? " (credit " + e.credit + ")" : ""))}</title><line x1="${20 + position * 8.6}" y1="8" x2="${20 + position * 8.6}" y2="34"/></g>`;
          })
          .join("");
        return `<div class="timeline-row"><div class="timeline-name">${escape(name(a))}</div><div><svg viewBox="0 0 900 42" class="timeline-track" role="img" aria-label="${escape(name(a))} reset timeline"><line x1="20" y1="21" x2="880" y2="21" class="chart-grid"/>${marks}</svg>${!events.length ? '<span class="muted">No observed refresh or expiry times</span>' : ""}${outside.length ? '<div class="timeline-outside">' + outside.map((e) => escape(e.label + ": " + absolute(e.at))).join("<br>") + "</div>" : ""}</div></div>`;
      })
      .join("");
    return `<div class="timeline-axis"><span></span><svg viewBox="0 0 900 30" aria-hidden="true">${axis}</svg></div>${rows || '<div class="empty-state">No accounts match these filters.</div>'}`;
  }
  const help = {
    automation: {
      off: "Never redeems a saved reset automatically. Dashboard collection or enabled routing can still observe quota.",
      notify:
        "Calculates reset advice without redeeming credits. You decide whether to act.",
      auto_expiring:
        "At confirmed weekly exhaustion, immediately uses the earliest eligible reset expiring before the next normal weekly refresh. Later-expiring resets stay saved. If allowance remains, the expiry guard is a safety fallback.",
      auto_all_saved:
        "Also permits saved non-expiring credits under demand and reserve rules. This is broader than expiring-only automation and can spend saved credits.",
    },
    fallback: {
      "round-robin":
        "Rotates among eligible accounts when deadline evidence is missing. Respects model availability and cooldowns.",
      "fill-first":
        "Uses the first eligible account until it becomes unavailable, then moves to another.",
      "weighted-round-robin":
        "Rotates among eligible accounts using their configured credential weights.",
    },
    affinity: {
      strict:
        "Keeps a conversation on its owning account. Earlier deadlines do not migrate that conversation.",
      deadline_at_boundary:
        "Can move replayable work between completed requests to an earlier deadline. Live streams, WebSockets and account-bound continuation state stay with their owner.",
    },
  };
  const helpers = {
    activityView,
    quotaView,
    pageAccounts,
    mergeAccounts,
    timelineEvents,
    eventPosition,
    chartSeries,
    createPoller,
    renderChart,
    renderTimeline,
    escape,
    number,
    help,
  };
  if (typeof module !== "undefined" && module.exports) module.exports = helpers;
  else root.PolicyDashboard = helpers;
})(typeof window === "undefined" ? globalThis : window);
