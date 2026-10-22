/* Permanent measurement history and shared temporary routing. */
(function (root) {
  "use strict";
  const esc = (value) =>
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
  function wall(at, zone) {
    const fixed = zone === "GMT+2";
    return Object.fromEntries(
      new Intl.DateTimeFormat("en-GB", {
        timeZone: fixed ? "UTC" : zone,
        year: "numeric",
        month: "2-digit",
        day: "2-digit",
        hour: "2-digit",
        minute: "2-digit",
        hourCycle: "h23",
      })
        .formatToParts(new Date(at + (fixed ? 7200000 : 0)))
        .filter((p) => p.type !== "literal")
        .map((p) => [p.type, Number(p.value)]),
    );
  }
  function days(now, offset = 0, zone = "Europe/Berlin") {
    const today = wall(now, zone),
      first = Date.UTC(today.year, today.month - 1, today.day - 6 + offset * 7);
    const boundary = (i) => {
      const target = first + i * 86400000;
      let at = target;
      for (let n = 0; n < 4; n++) {
        const w = wall(at, zone);
        at += target - Date.UTC(w.year, w.month - 1, w.day, w.hour, w.minute);
      }
      return at;
    };
    return Array.from({ length: 7 }, (_, i) => ({
      start: boundary(i),
      end: boundary(i + 1),
    }));
  }
  function hour(at, zone) {
    if (zone === "GMT+2")
      return (
        new Intl.DateTimeFormat("en-GB", {
          timeZone: "UTC",
          hour: "2-digit",
          minute: "2-digit",
          hourCycle: "h23",
        }).format(new Date(at + 7200000)) + " GMT+2"
      );
    return new Intl.DateTimeFormat("en-GB", {
      timeZone: zone,
      hour: "2-digit",
      minute: "2-digit",
      hourCycle: "h23",
      timeZoneName: "shortOffset",
    }).format(new Date(at));
  }
  function render(series, rows, zone = "Europe/Berlin") {
    const width = 960,
      left = 54,
      right = 945,
      top = 26,
      bottom = 106,
      x = (t, row) =>
        left + ((t - row.start) / (row.end - row.start)) * (right - left),
      y = (v) =>
        bottom - (Math.max(0, Math.min(100, v)) * (bottom - top)) / 100;
    return rows
      .map((row) => {
        let svg = "";
        for (const v of [0, 50, 100])
          svg += `<line class="history-grid" x1="${left}" x2="${right}" y1="${y(v)}" y2="${y(v)}"/><text x="2" y="${y(v) + 4}">${v}%</text>`;
        for (let t = row.start; t < row.end; t += 3600000) {
          const label = hour(t, zone),
            w = wall(t, zone);
          svg += `<line class="history-grid" x1="${x(t, row)}" x2="${x(t, row)}" y1="${top}" y2="${bottom}"><title>${esc(label)}</title></line>`;
          if (w.hour % 6 === 0 || w.hour === 2)
            svg += `<text x="${x(t, row)}" y="15">${esc(label)}</text>`;
        }
        for (const s of series) {
          const points = (s.observations || [])
            .filter(
              (p) =>
                Date.parse(p.observed_at) >= row.start &&
                Date.parse(p.observed_at) < row.end &&
                !p.model &&
                (!p.scope || p.scope === "ordinary"),
            )
            .sort(
              (a, b) => Date.parse(a.observed_at) - Date.parse(b.observed_at),
            );
          let path = "",
            previous = null;
          const paths = [];
          for (const p of points) {
            const t = Date.parse(p.observed_at),
              v = Number(p.remaining_percent);
            if (!Number.isFinite(v)) continue;
            const px = x(t, row),
              py = y(v);
            if (
              previous &&
              (t - Date.parse(previous.observed_at) > 600000 ||
                p.event === "identity_change")
            ) {
              paths.push(path);
              path = "";
            }
            if (!path) path = `M${px},${py}`;
            else if (
              ["manual_reset", "natural_refill", "allowance_change"].includes(
                p.event,
              )
            )
              path += ` H${px} V${py}`;
            else path += ` L${px},${py}`;
            const mark = [
              "manual_reset",
              "natural_refill",
              "allowance_change",
            ].includes(p.event);
            svg += `<circle class="history-point ${mark ? "history-event" : ""} ${mark ? "event-" + esc(p.event) : ""}" cx="${px}" cy="${py}" r="${mark ? 4 : 2}" fill="${colour(s.id)}" tabindex="0"><title>${esc(s.name)} · ${esc(new Date(t).toISOString())} · ${v}% remaining · ${esc(p.event)}</title></circle>`;
            previous = p;
          }
          if (path) paths.push(path);
          svg += paths
            .map(
              (d) =>
                `<path d="${d}" fill="none" stroke="${colour(s.id)}" stroke-width="2"/>`,
            )
            .join("");
          // A reset timestamp is an expectation, never an observed allowance line.
          const last = (s.observations || [])
            .filter((p) => p.reset_at)
            .sort(
              (a, b) => Date.parse(a.observed_at) - Date.parse(b.observed_at),
            )
            .at(-1);
          const reset = Date.parse(last?.reset_at);
          if (reset > Date.now() && reset >= row.start && reset < row.end)
            svg += `<line class="history-expected" x1="${x(reset, row)}" x2="${x(reset, row)}" y1="${top}" y2="${bottom}"><title>${esc(s.name)} expected reset · ${esc(last.reset_at)}; recovery unconfirmed</title></line>`;
        }
        const date = new Intl.DateTimeFormat("en-GB", {
          timeZone: zone === "GMT+2" ? "UTC" : zone,
          weekday: "short",
          day: "2-digit",
          month: "short",
          year: "numeric",
        }).format(new Date(row.start + (zone === "GMT+2" ? 7200000 : 0)));
        return `<div class="allowance-day"><strong>${esc(date)}</strong><svg viewBox="0 0 ${width} 122" role="img" aria-label="${esc(date)} remaining allowance, 0 to 100 percent, ${(row.end - row.start) / 3600000} hours">${svg}</svg></div>`;
      })
      .join("");
  }
  function colour(id) {
    let hash = 0;
    for (const c of String(id)) hash = (hash * 31 + c.charCodeAt(0)) >>> 0;
    return ["#247f9e", "#b65d22", "#7d5ab1", "#18816c", "#c04569", "#5f7c20"][
      hash % 6
    ];
  }
  function table(headers, rows) {
    return `<div class="history-table-scroll"><table><thead><tr>${headers.map((h) => `<th scope="col">${esc(h)}</th>`).join("")}</tr></thead><tbody>${rows.map((row) => `<tr>${row.map((v) => `<td>${esc(v ?? "Unavailable")}</td>`).join("")}</tr>`).join("")}</tbody></table></div>`;
  }
  function createController(options) {
    const $ = (id) => document.getElementById(id);
    let revision = null,
      override = null,
      mutationBusy = false,
      uncertain = false,
      offset = 0,
      refreshID = 0,
      requestID = 0,
      cursor = "",
      nextCursor = "",
      back = [],
      series = [],
      dayRows = [],
      generation = 0,
      summaryID = 0,
      lastAccounts = "";
    const accountName = (id) => {
      const a = options.accounts().find((a) => a.identity.credential_id === id);
      return a ? options.alias(a) : id;
    };
    function renderOverride() {
      if (!override) return;
      const active = override.state === "active";
      $("override-status").textContent = active
        ? `${accountName(override.credential_id)} · expires ${options.instant(override.expires_at, options.zone())} · ${options.countdown(override.expires_at, Date.now())}`
        : `Smart routing · ${override.termination_reason || override.state}`;
      $("override-apply").disabled =
        mutationBusy || uncertain || revision === null;
      $("override-cancel").disabled = mutationBusy || uncertain || !active;
    }
    async function readOverride() {
      const epoch = generation;
      const value = await options.request("/routing-override");
      if (
        epoch !== generation ||
        (revision !== null && value.revision < revision)
      )
        return;
      override = value;
      revision = value.revision;
      uncertain = false;
      renderOverride();
    }
    async function mutate(cancel) {
      if (mutationBusy || uncertain || revision === null) return;
      mutationBusy = true;
      const epoch = generation;
      renderOverride();
      try {
        const value = await options.request(
          "/routing-override" +
            (cancel ? "?expected_revision=" + revision : ""),
          cancel ? "DELETE" : "PUT",
          cancel
            ? undefined
            : {
                credential_id: $("override-account").value,
                duration_hours: Number($("override-hours").value),
                expected_revision: revision,
              },
        );
        if (epoch !== generation) return;
        if (revision === null || value.revision >= revision) {
          override = value;
          revision = value.revision;
        }
        options.notice(
          cancel
            ? "Smart routing resumed."
            : "Temporary routing applied across callers.",
        );
      } catch (e) {
        uncertain = true;
        options.notice(
          e.message + " Reading shared state before another change.",
          true,
        );
        try {
          await readOverride();
        } catch (_) {
          options.notice(
            "Shared routing state unavailable. Reload before changing routing again.",
            true,
          );
        }
      } finally {
        mutationBusy = false;
        renderOverride();
      }
    }
    function choices() {
      const accounts = options.accounts();
      const fingerprint = JSON.stringify(
        accounts.map((a) => [a.identity.credential_id, options.alias(a)]),
      );
      if (fingerprint === lastAccounts) return;
      lastAccounts = fingerprint;
      for (const id of [
        "override-account",
        "allowance-account",
        "history-account",
      ]) {
        const selected = $(id).value,
          first =
            id === "override-account"
              ? "Select account"
              : id === "allowance-account"
                ? "Accounts on filtered page"
                : "All accounts";
        $(id).innerHTML =
          `<option value="">${first}</option>` +
          accounts
            .map(
              (a) =>
                `<option value="${esc(a.identity.credential_id)}">${esc(options.alias(a))}</option>`,
            )
            .join("");
        if (accounts.some((a) => a.identity.credential_id === selected))
          $(id).value = selected;
      }
    }
    function draw() {
      if (!dayRows.length) return;
      $("allowance-history-chart").innerHTML = render(
        series,
        dayRows,
        options.zone(),
      );
      $("allowance-history-values").innerHTML = table(
        [
          "Account",
          "Timestamp (UTC)",
          "Remaining",
          "Event",
          "Reset expected (UTC)",
        ],
        series.flatMap((s) =>
          s.observations.map((p) => [
            s.name,
            p.observed_at,
            p.remaining_percent + "%",
            p.event,
            p.reset_at,
          ]),
        ),
      );
      $("allowance-legend").innerHTML = series
        .map(
          (s) =>
            `<span><svg width="16" height="12" aria-hidden="true"><line x1="0" x2="16" y1="6" y2="6" stroke="${colour(s.id)}" stroke-width="3"/></svg> ${esc(s.name)}</span>`,
        )
        .join("");
    }
    async function allowances() {
      if (document.hidden) return;
      const id = ++refreshID;
      dayRows = days(Date.now(), offset, options.zone());
      const selected = $("allowance-account").value;
      const accounts = selected
        ? options
            .accounts()
            .filter((a) => a.identity.credential_id === selected)
        : options.filtered();
      const range = {
        from: new Date(dayRows[0].start).toISOString(),
        to: new Date(dayRows.at(-1).end).toISOString(),
        window: $("allowance-window").value,
      };
      $("allowance-history-chart").setAttribute("aria-busy", "true");
      try {
        const results = [];
        let queue = accounts.slice();
        await Promise.all(
          Array.from({ length: Math.min(4, accounts.length) }, async () => {
            while (queue.length) {
              const a = queue.shift(),
                q = new URLSearchParams({
                  ...range,
                  credential_id: a.identity.credential_id,
                });
              const data = await options.request("/allowance-history?" + q);
              results.push({
                id: a.identity.credential_id,
                name: options.alias(a),
                observations: data.observations || [],
                status: data.status,
              });
            }
          }),
        );
        if (id !== refreshID) return;
        series = results;
        draw();
        const total = series.reduce((n, s) => n + s.observations.length, 0),
          disabled = series.some((s) => !s.status?.enabled),
          unhealthy = series.some((s) => !s.status?.healthy);
        $("allowance-history-state").textContent = !accounts.length
          ? "No accounts match these filters."
          : !total
            ? disabled
              ? "Collection disabled. Retained history is empty."
              : "Waiting for quota observations in this period."
            : unhealthy
              ? "Partial coverage: storage errors or damaged segments."
              : disabled
                ? "Collection disabled; displaying retained observations."
                : "Observed samples only. Blank intervals are missing coverage. Confirmed manual resets: filled marker; natural refills: ring; expected reset: dashed guide. Unexplained increases are allowance changes.";
      } catch (e) {
        if (id === refreshID) {
          $("allowance-history-state").textContent =
            "Allowance history unavailable: " + e.message;
          $("allowance-history-chart").replaceChildren();
        }
      } finally {
        if (id === refreshID)
          $("allowance-history-chart").setAttribute("aria-busy", "false");
      }
    }
    function query() {
      const q = new URLSearchParams();
      const from = $("history-from").value,
        to = $("history-to").value;
      if (from) q.set("from", new Date(from + "T00:00:00Z").toISOString());
      if (to)
        q.set(
          "to",
          new Date(Date.parse(to + "T00:00:00Z") + 86400000).toISOString(),
        );
      for (const [field, id] of [
        ["provider", "history-provider"],
        ["model", "history-model"],
        ["credential_id", "history-account"],
      ])
        if ($(id).value) q.set(field, $(id).value);
      return q;
    }
    async function requests(reset = false) {
      if (document.hidden) return;
      if (reset) {
        cursor = "";
        back = [];
      }
      const id = ++requestID;
      const q = query();
      if (cursor) q.set("cursor", cursor);
      q.set("limit", "100");
      try {
        const data = await options.request("/usage-history/requests?" + q);
        if (id !== requestID) return;
        nextCursor = data.next_cursor || "";
        $("request-history").innerHTML = data.records?.length
          ? table(
              [
                "Time (UTC)",
                "Requested model",
                "Execution model",
                "Response model",
                "Account",
                "Outcome",
                "Input",
                "Output",
                "Cached input",
                "Quality",
              ],
              data.records.map((r) => [
                r.requested_at,
                r.requested_model || "Unknown",
                r.execution_model || "Unknown",
                r.response_model || "Unknown",
                accountName(r.credential_id),
                r.outcome,
                r.tokens?.input?.total_tokens,
                r.tokens?.output?.total_tokens,
                r.cached_input_percent == null
                  ? "Unavailable"
                  : r.cached_input_percent.toFixed(2) + "%",
                r.measurement_quality || r.tokens?.quality || "Unknown",
              ]),
            )
          : "No provider attempts in this period.";
        $("history-next").disabled = !nextCursor;
        $("history-previous").disabled = !back.length;
      } catch (e) {
        if (id === requestID)
          $("request-history").textContent =
            "Request history unavailable: " + e.message;
      }
    }
    async function summaries() {
      const id = ++summaryID,
        q = query();
      q.set("group", $("history-group").value);
      try {
        const data = await options.request("/usage-history/summary?" + q);
        if (id !== summaryID) return;
        $("history-summary").innerHTML = data.groups?.length
          ? table(
              [
                "Period (UTC)",
                "Provider",
                "Execution model",
                "Account",
                "Attempts",
                "Input",
                "Output",
                "Cached input",
                "Measured attempts",
              ],
              data.groups.map((t) => [
                t.period,
                t.provider,
                t.model || "Unknown",
                accountName(t.credential_id),
                t.provider_attempts,
                t.input_tokens,
                t.output_tokens,
                t.cached_input_percent == null
                  ? "Unavailable"
                  : t.cached_input_percent.toFixed(2) + "%",
                `${t.cache_measured_attempts}/${t.provider_attempts}`,
              ]),
            )
          : "No retained measurements in this period.";
      } catch (e) {
        if (id !== summaryID) return;
        $("history-summary").textContent = "Summary unavailable: " + e.message;
      }
    }
    async function refresh() {
      if (document.hidden) return;
      choices();
      await Promise.allSettled([
        readOverride().catch((e) => {
          revision = null;
          renderOverride();
          $("override-status").textContent =
            "Routing state unavailable: " + e.message;
        }),
        allowances(),
        requests(),
        summaries(),
        options
          .request("/usage-history/status")
          .then((s) => {
            const gaps = s.uncertain_coverage?.length || 0;
            $("history-health").textContent =
              `Long-term logging ${s.enabled ? "enabled" : "disabled"} · ${s.healthy ? "storage healthy" : "storage unhealthy"} · ${s.write_errors || 0} write errors · ${gaps} coverage gap${gaps === 1 ? "" : "s"}. ${s.coverage_note || ""}`;
          })
          .catch((e) => {
            $("history-health").textContent =
              "Long-term history unavailable: " + e.message;
          }),
      ]);
    }
    $("override-hours").innerHTML = Array.from(
      { length: 24 },
      (_, i) =>
        `<option value="${i + 1}">${i + 1} ${i ? "hours" : "hour"}</option>`,
    ).join("");
    $("override-apply").addEventListener("click", () => mutate(false));
    $("override-cancel").addEventListener("click", () => mutate(true));
    for (const id of ["allowance-account", "allowance-window"])
      $(id).addEventListener("change", allowances);
    for (const [id, step] of [
      ["allowance-previous", -1],
      ["allowance-next", 1],
    ])
      $(id).addEventListener("click", () => {
        offset += step;
        allowances();
      });
    $("allowance-today").addEventListener("click", () => {
      offset = 0;
      allowances();
    });
    $("history-from").value = new Date(Date.now() - 30 * 86400000)
      .toISOString()
      .slice(0, 10);
    $("history-to").value = new Date().toISOString().slice(0, 10);
    for (const id of [
      "history-from",
      "history-to",
      "history-provider",
      "history-model",
      "history-account",
    ])
      $(id).addEventListener("change", () => {
        requests(true);
        summaries();
      });
    $("history-group").addEventListener("change", summaries);
    $("history-next").addEventListener("click", () => {
      if (!nextCursor) return;
      back.push(cursor);
      cursor = nextCursor;
      requests();
    });
    $("history-previous").addEventListener("click", () => {
      cursor = back.pop() || "";
      requests();
    });
    for (const format of ["csv", "ndjson"])
      $("history-export-" + format).addEventListener("click", () => {
        const q = query();
        q.set("format", format);
        options
          .download("/usage-history/export?" + q, format)
          .catch((e) => options.notice(e.message, true));
      });
    return {
      refresh,
      allowances,
      tick: renderOverride,
      reset() {
        generation++;
        refreshID++;
        requestID++;
        summaryID++;
        override = null;
        revision = null;
        cursor = "";
        nextCursor = "";
        back = [];
        series = [];
        lastAccounts = "";
        for (const id of [
          "request-history",
          "history-summary",
          "allowance-history-chart",
          "allowance-history-values",
        ])
          $(id).replaceChildren();
      },
    };
  }
  const api = { days, render, createController };
  if (typeof module !== "undefined" && module.exports) module.exports = api;
  else root.PolicyHistory = api;
})(typeof window !== "undefined" ? window : globalThis);
