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
  // Subtract decimal provider percentages without rounding or binary float noise.
  function remainingPercentage(used) {
    const [coefficient, exponent = "0"] = String(used).split("e");
    const fractional = (coefficient.split(".")[1] || "").length;
    const decimals = Math.max(0, fractional - Number(exponent));
    const scale = 10n ** BigInt(decimals);
    const units =
      BigInt(coefficient.replace(".", "")) *
      10n ** BigInt(Math.max(0, Number(exponent) - fractional));
    const remaining = (100n * scale - units)
      .toString()
      .padStart(decimals + 1, "0");
    if (!decimals) return remaining;
    const fraction = remaining.slice(-decimals).replace(/0+$/, "");
    return remaining.slice(0, -decimals) + (fraction ? "." + fraction : "");
  }
  // Display evidence may outlive the stricter backend action freshness window.
  function quotaView(account, settings, now, duration = 604800) {
    const validTime = (value) => {
      const at = stamp(value);
      return at !== null && at <= now ? at : null;
    };
    const bucket = (account.buckets || []).find(
      (b) =>
        b.duration_seconds === duration &&
        !b.model &&
        (!b.scope || b.scope === "ordinary"),
    );
    const at = validTime(bucket?.observed_at || account.observed_at);
    const inventoryAt = validTime(account.inventory_observed_at);
    const limit = (settings.freshness_seconds || 120) * 1000;
    const validAllowance =
      bucket &&
      at !== null &&
      measured(bucket.used_percent) &&
      bucket.used_percent <= 100;
    return {
      remaining: validAllowance ? 100 - bucket.used_percent : null,
      remainingLabel: validAllowance
        ? remainingPercentage(bucket.used_percent)
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
    return quotaCycle(bucket, now, start, end, 604800);
  }
  function quotaCycle(bucket, now, start, end, duration) {
    const reset = stamp(bucket.reset_at);
    if (
      bucket.duration_seconds !== duration ||
      bucket.model ||
      (bucket.scope && bucket.scope !== "ordinary") ||
      reset === null
    )
      return null;
    const cycleStart = reset - duration * 1000;
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
  function timelineRange(
    now,
    mode = "weekly",
    offset = 0,
    zone = "Europe/Berlin",
  ) {
    offset = Math.max(-52, Math.min(52, Math.trunc(Number(offset) || 0)));
    const fixed = zone === "GMT+2";
    const timeZone = fixed ? "UTC" : zone;
    const formatter = new Intl.DateTimeFormat("en-GB", {
      timeZone,
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
      hour: "2-digit",
      minute: "2-digit",
      hourCycle: "h23",
    });
    const wall = (at) =>
      Object.fromEntries(
        formatter
          .formatToParts(new Date(at + (fixed ? 7200000 : 0)))
          .filter((p) => p.type !== "literal")
          .map((p) => [p.type, Number(p.value)]),
      );
    if (mode === "5h") {
      const start =
        Math.floor(now / 3600000) * 3600000 -
        6 * 3600000 +
        offset * 12 * 3600000;
      return {
        start,
        end: start + 12 * 3600000,
        ticks: Array.from({ length: 12 }, (_, i) => ({
          at: start + i * 3600000,
          end: start + (i + 1) * 3600000,
        })),
        mode,
      };
    }
    const today = wall(now);
    const calendar = Date.UTC(
      today.year,
      today.month - 1,
      today.day - 7 + offset * 14,
    );
    const midnight = (day) => {
      const target = calendar + day * 86400000;
      let at = target;
      // Solve local midnight against the actual offset on each date, not today's
      // offset. DST days can be 23/25 hours while quota durations stay exact.
      for (let i = 0; i < 3; i++) {
        const parts = wall(at);
        const local = Date.UTC(
          parts.year,
          parts.month - 1,
          parts.day,
          parts.hour,
          parts.minute,
        );
        at += target - local;
      }
      return at;
    };
    const boundaries = Array.from({ length: 15 }, (_, i) => midnight(i));
    return {
      start: boundaries[0],
      end: boundaries[14],
      ticks: boundaries
        .slice(0, 14)
        .map((at, i) => ({ at, end: boundaries[i + 1] })),
      mode: "weekly",
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
  function renderTimeline(
    accounts,
    settings,
    now,
    days,
    absolute,
    options = {},
  ) {
    const zone = options.zone || "Europe/Berlin";
    const mode = options.mode === "5h" ? "5h" : "weekly";
    const duration = mode === "5h" ? 18000 : 604800;
    const { start, end, ticks } = timelineRange(
      now,
      mode,
      options.offset,
      zone,
    );
    const x = (at) => (100 * (at - start)) / (end - start);
    const pct = (value) => `${value.toFixed(6)}%`;
    const local = (at, format) =>
      new Intl.DateTimeFormat("en-GB", {
        timeZone: zone === "GMT+2" ? "UTC" : zone,
        ...format,
      }).format(new Date(at + (zone === "GMT+2" ? 7200000 : 0)));
    const shortDate = (at) =>
      local(at, {
        day: "2-digit",
        month: "2-digit",
        hour: "2-digit",
        minute: "2-digit",
        hourCycle: "h23",
      });
    const nowLine = (height) =>
      now >= start && now <= end
        ? `<line x1="${pct(x(now))}" x2="${pct(x(now))}" y1="0" y2="${height}" class="timeline-now"><title>Now: ${escape(absolute(new Date(now).toISOString()))}</title></line>`
        : "";
    const grid = (height) =>
      ticks
        .map(
          (t) =>
            `<line x1="${pct(x(t.at))}" x2="${pct(x(t.at))}" y1="0" y2="${height}" class="timeline-day-grid"/>`,
        )
        .join("");
    const axis = ticks
      .map((t) => {
        const center = pct(x((t.at + t.end) / 2));
        const today = now >= t.at && now < t.end;
        const primary =
          mode === "weekly"
            ? local(t.at, { day: "2-digit", month: "2-digit" })
            : local(t.at, {
                hour: "2-digit",
                minute: "2-digit",
                hourCycle: "h23",
              });
        return `<g class="timeline-day${today ? " today" : ""}"><title>${escape(absolute(new Date(t.at).toISOString()))}</title><text x="${center}" y="15" text-anchor="middle" class="timeline-day-weekday">${escape(local(t.at, { weekday: "short" }))}</text><text x="${center}" y="34" text-anchor="middle" class="timeline-day-date">${escape(primary)}</text><text x="${center}" y="34" text-anchor="middle" class="timeline-day-mobile">${escape(local(t.at, mode === "weekly" ? { day: "2-digit" } : { hour: "2-digit", hourCycle: "h23" }))}</text></g>`;
      })
      .join("");
    const rows = accounts
      .map((a, index) => {
        const events = timelineEvents(a, settings, now);
        const inside = events.filter(
          (e) =>
            e.kind !== "weekly" && !eventPosition(e.at, start, end).outside,
        );
        const later = events.filter(
          (e) => e.kind === "expiry" && stamp(e.at) > end,
        );
        const marker = (e) => {
          const position = pct(x(stamp(e.at)));
          return `<g class="timeline-marker ${e.kind}" tabindex="0" data-focus-key="${escape(a.identity.credential_id + ":" + e.kind + ":" + (e.credit || "") + ":" + e.at)}" role="img" aria-label="${escape(e.label + ": " + absolute(e.at))}"><title>${escape(e.label + ": " + absolute(e.at) + (e.credit ? " (credit " + e.credit + ")" : ""))}</title><line x1="${position}" y1="3" x2="${position}" y2="19"/></g>`;
        };
        const manualMarks = inside
          .filter((e) => e.kind !== "window")
          .map(marker)
          .join("");
        const bands = (a.buckets || [])
          .map((b, bucketIndex) => {
            const e = quotaCycle(b, now, start, end, duration);
            if (!e) return "";
            const quota = quotaView(
              { ...a, buckets: [b] },
              settings,
              now,
              duration,
            );
            const allowance =
              quota.remaining === null
                ? "Allowance unavailable"
                : `${quota.remainingLabel}% left${quota.fresh ? "" : " (last known)"}`;
            const label = `${mode === "weekly" ? "Seven-day weekly cycle" : "Five-hour window"}: ${absolute(new Date(e.start).toISOString())} to ${absolute(new Date(e.end).toISOString())}. Start inferred from provider refresh and duration. ${allowance}. Width and shading show time, not quota consumed.`;
            const left = x(e.visibleStart),
              width = x(e.visibleEnd) - left;
            const clip = `timeline-clip-${index}-${bucketIndex}`;
            const futureEnd = Math.min(end, e.end + duration * 1000);
            const projectedLabel = `Estimated next window: ${absolute(new Date(e.end).toISOString())} to ${absolute(new Date(e.end + duration * 1000).toISOString())}. Not observed; manual resets may shift it.`;
            const future =
              e.end > now && futureEnd > e.end
                ? `<g class="timeline-projection" tabindex="0" data-focus-key="${escape(a.identity.credential_id + ":projected:" + duration + ":" + e.end)}" role="img" aria-label="${escape(projectedLabel)}"><title>${escape(projectedLabel)}</title><rect x="${pct(x(e.end))}" y="9" width="${pct(x(futureEnd) - x(e.end))}" height="24" rx="12"/></g>`
                : "";
            return `${future}<g class="weekly-cycle" tabindex="0" data-focus-key="${escape(a.identity.credential_id + ":cycle:" + duration + ":" + e.end)}" role="img" aria-label="${escape(label)}"><title>${escape(label)}</title><defs><clipPath id="${clip}"><rect x="${pct(left)}" y="9" width="${pct(width)}" height="24" rx="12"/></clipPath></defs><rect x="${pct(left)}" y="9" width="${pct(width)}" height="24" rx="12" class="weekly-cycle-remaining"/>${e.elapsedEnd > e.visibleStart ? `<rect x="${pct(left)}" y="9" width="${pct(x(e.elapsedEnd) - left)}" height="24" clip-path="url(#${clip})" class="weekly-cycle-elapsed"/>` : ""}<rect x="${pct(left)}" y="9" width="${pct(width)}" height="24" rx="12" class="weekly-cycle-outline"/><text x="${pct(left)}" dx="8" y="25" clip-path="url(#${clip})" class="timeline-cycle-label">${escape(allowance + " · " + shortDate(e.end))}</text></g>`;
          })
          .join("");
        const quota = quotaView(a, settings, now, duration);
        const description =
          quota.remaining === null
            ? "Allowance unavailable"
            : `${quota.remainingLabel}% left${quota.fresh ? "" : " (last known)"}`;
        const resets = manualMarks
          ? `<div class="timeline-reset-lane"><span class="timeline-lane-label">Saved manual resets</span><svg class="timeline-track timeline-manual" role="img" aria-label="${escape(name(a))} manual reset expiry">${grid(22)}${manualMarks}${nowLine(22)}</svg></div>`
          : "";
        return `<div class="timeline-row"><div class="timeline-name"><button type="button" class="timeline-account" data-focus-key="${escape(a.identity.credential_id + ":timeline-account")}" data-open-account="${escape(a.identity.credential_id)}" title="${escape(name(a))}">${escape(name(a))}</button><span class="timeline-duration">${mode === "weekly" ? "7d" : "5h"}</span><small>${escape(description)}${stamp(quota.resetAt) !== null ? `<span class="timeline-refresh-label"> · ${escape(shortDate(stamp(quota.resetAt)))}</span>` : ""}</small></div><div class="timeline-lanes"><div class="timeline-lane"><svg class="timeline-track timeline-weekly" role="img" aria-label="${escape(name(a))} ${mode === "weekly" ? "Weekly allowance" : "Five-hour window"}">${grid(42)}${bands}${nowLine(42)}</svg>${!bands ? '<span class="timeline-missing">No observed window in this range</span>' : ""}</div>${resets}${later.length ? `<button type="button" class="timeline-later" data-focus-key="${escape(a.identity.credential_id + ":timeline-later")}" data-open-account="${escape(a.identity.credential_id)}">${later.length} later ${later.length === 1 ? "expiry" : "expiries"} · View account</button>` : ""}</div></div>`;
      })
      .join("");
    return `<div class="timeline-table"><div class="timeline-axis"><span>Account</span><svg class="timeline-axis-track" role="img" aria-label="${escape(mode === "weekly" ? "Daily date columns" : "Hourly time columns")}">${grid(44)}${axis}</svg></div>${rows || '<div class="empty-state">No accounts match these filters.</div>'}</div>`;
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
    quotaCycle,
    timelineRange,
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
