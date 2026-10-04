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
  function weeklyCycle(bucket, now, start, end) {
    const reset = stamp(bucket.reset_at);
    if (
      bucket.duration_seconds !== 604800 ||
      bucket.model ||
      (bucket.scope && bucket.scope !== "ordinary") ||
      reset === null
    )
      return null;
    const cycleStart = reset - 604800000;
    const visibleStart = Math.max(start, cycleStart),
      visibleEnd = Math.min(end, reset);
    if (visibleEnd <= visibleStart) return null;
    return {
      start: cycleStart,
      end: reset,
      visibleStart,
      visibleEnd,
      elapsedEnd: Math.max(visibleStart, Math.min(now, visibleEnd)),
    };
  }
  function renderAllowances(accounts, settings, now, absolute) {
    return (
      accounts
        .map((a) => {
          const quota = quotaView(a, settings, now);
          const label =
            quota.remaining === null
              ? "No weekly observation"
              : quota.fresh
                ? "Weekly allowance left"
                : "Last known weekly allowance";
          return `<button type="button" class="allowance-account" data-open-account="${escape(a.identity.credential_id)}"><span><strong>${escape(name(a))}</strong><small>${escape(a.identity.provider || "")} · ${escape(label)}</small></span><span class="allowance-value">${quota.remaining === null ? "Unavailable" : escape(number(quota.remaining)) + "%"}<small>${quota.remaining === null ? "Awaiting quota observation" : "Automatic refresh: " + escape(absolute(quota.resetAt))}</small></span></button>`;
        })
        .join("") ||
      '<p class="muted">No accounts match the current filters.</p>'
    );
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
    // Include the past week so elapsed cycle time is visible, not clipped at Now.
    const start = now - 604800000;
    const end = now + (days === 1 ? 1 : 7) * 86400000;
    const x = (at) => 20 + ((at - start) / (end - start)) * 860;
    const ticks = [start, now, end];
    const axis = ticks
      .map(
        (at) =>
          `<span class="timeline-tick ${at === start ? "start" : at === end ? "end" : "now"}">${escape(at === now ? "Now" : absolute(new Date(at).toISOString()).replace(/(\d{2}\/\d{2})\/\d{4}, (\d{2}:\d{2}).*/, "$1 $2"))}</span>`,
      )
      .join("");
    const rows = accounts
      .map((a) => {
        const events = timelineEvents(a, settings, now);
        const inside = events.filter(
          (e) =>
            e.kind !== "weekly" && !eventPosition(e.at, start, end).outside,
        );
        const later = events.filter(
          (e) => e.kind === "expiry" && stamp(e.at) > end,
        );
        const marker = (e) => {
          const position = x(stamp(e.at));
          return `<g class="timeline-marker ${e.kind}" tabindex="0" data-focus-key="${escape(a.identity.credential_id + ":" + e.kind + ":" + (e.credit || "") + ":" + e.at)}" role="img" aria-label="${escape(e.label + ": " + absolute(e.at))}"><title>${escape(e.label + ": " + absolute(e.at) + (e.credit ? " (credit " + e.credit + ")" : ""))}</title><line x1="${position}" y1="8" x2="${position}" y2="34"/></g>`;
        };
        const manualMarks = inside
          .filter((e) => e.kind !== "window")
          .map(marker)
          .join("");
        const windowMarks = inside
          .filter((e) => e.kind === "window")
          .map(marker)
          .join("");
        const bands = (a.buckets || [])
          .map((b) => {
            const e = weeklyCycle(b, now, start, end);
            if (!e) return "";
            const quota = quotaView({ ...a, buckets: [b] }, settings, now);
            const height =
              quota.remaining === null ? null : quota.remaining * 0.4;
            const allowance =
              height === null
                ? "Weekly allowance unavailable"
                : `${number(quota.remaining)}% left${quota.fresh ? "" : " (last known)"}`;
            const label = `Seven-day weekly cycle: ${absolute(new Date(e.start).toISOString())} to ${absolute(new Date(e.end).toISOString())}. Start inferred from provider refresh and seven-day duration. ${allowance}. Fill height shows allowance left; the top strip shows elapsed time.`;
            const left = x(e.visibleStart),
              width = x(e.visibleEnd) - left;
            return `<g class="weekly-cycle" tabindex="0" data-focus-key="${escape(a.identity.credential_id + ":cycle:" + e.end)}" role="img" aria-label="${escape(label)}"><title>${escape(label)}</title><rect x="${left}" y="14" width="${width}" height="40" class="weekly-cycle-remaining"/>${height !== null ? `<rect x="${left}" y="${54 - height}" width="${width}" height="${height}" class="weekly-cycle-quota"/>` : ""}<rect x="${left}" y="6" width="${width}" height="4" class="weekly-cycle-remaining"/>${e.elapsedEnd > e.visibleStart ? `<rect x="${left}" y="6" width="${x(e.elapsedEnd) - left}" height="4" class="weekly-cycle-elapsed"/>` : ""}<rect x="${left}" y="14" width="${width}" height="40" class="weekly-cycle-outline"/></g>`;
          })
          .join("");
        const nowLine = `<line x1="${x(now)}" x2="${x(now)}" y1="3" y2="39" class="timeline-now"/>`;
        const quota = quotaView(a, settings, now);
        return `<div class="timeline-row"><div class="timeline-name">${escape(name(a))}${quota.remaining !== null ? `<small>${escape(number(quota.remaining))}% left${quota.fresh ? "" : " (last known)"}</small>` : ""}</div><div class="timeline-lanes"><div class="timeline-lane"><span class="timeline-lane-label">Weekly allowance</span><svg viewBox="0 0 900 64" preserveAspectRatio="none" class="timeline-track timeline-weekly" role="img" aria-label="${escape(name(a))} automatic allowance refresh">${bands}${windowMarks}<line x1="${x(now)}" x2="${x(now)}" y1="3" y2="61" class="timeline-now"/></svg>${!bands ? '<span class="muted">No observed weekly cycle in this range</span>' : ""}</div><div class="timeline-lane"><span class="timeline-lane-label">Saved manual resets</span><svg viewBox="0 0 900 42" preserveAspectRatio="none" class="timeline-track timeline-manual" role="img" aria-label="${escape(name(a))} manual reset expiry"><line x1="20" y1="21" x2="880" y2="21" class="chart-grid"/>${manualMarks}${nowLine}</svg>${!inside.some((e) => e.kind === "expiry") ? '<span class="muted">No expiry in this range</span>' : ""}${later.length ? `<button type="button" class="timeline-later" data-open-account="${escape(a.identity.credential_id)}">${later.length} later ${later.length === 1 ? "expiry" : "expiries"} · View account</button>` : ""}</div></div></div>`;
      })
      .join("");
    return `<div class="timeline-axis"><span></span><div class="timeline-ticks ${days === 1 ? "day" : "week"}">${axis}</div></div>${rows || '<div class="empty-state">No accounts match these filters.</div>'}`;
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
    weeklyCycle,
    renderAllowances,
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
