// quack usage dashboard: vanilla JS, no build step, no CDN. Charts are hand-rolled inline SVG;
// a few series over a few ranges doesn't earn a ~40KB charting library.

// Every metric/label string this page queries; mirrors quack's internal/otelobs/metrics.go.
// OTel surfaces to Prometheus as "<dotted_name_with_underscores>[_unit]_total|_bucket|_sum|_count".
const METRICS = {
  tokenUsage: "gen_ai_client_token_usage_total",
  // The collector appends the unit (gen_ai_client_cost_USD_total); match both spellings so a
  // unit-stripping exporter config still works.
  costNameRegex: 'gen_ai_client_cost(_USD)?_total',
  labels: {
    // Exporters sometimes surface gen_ai_request_model unqualified as "model": group by both and
    // resolve whichever is non-empty (firstLabel).
    model: "gen_ai_request_model",
    modelFallback: "model",
    source: "source",
    agent: "agent",
    user: "user",
    // Same hazard: gen_ai.token.type lands as gen_ai_token_type; keep the unqualified fallback.
    tokenType: "gen_ai_token_type",
    tokenTypeFallback: "token_type",
  },
  tokenTypeCached: "cached",
  tokenTypeInput: "input",
  tokenTypeOutput: "output",
};

// TOKEN_TYPES is a fixed display order so "cached"/"input" keep one color across panels
// whichever fetch resolves first.
const TOKEN_TYPES = ["input", "output", "reasoning", "cached"];

// LATENCY is quack.model.call.duration (a histogram, unit "s"). The page probes for its
// "_bucket" series before claiming a p50/p95/p99 chart.
const LATENCY = {
  metric: "quack_model_call_duration_seconds",
};

// Below this many NON-ZERO points an additive chart draws columns: a line through the zeros
// between a few daily runs implies traffic that never happened.
const SPARSE_MAX_NONZERO = 8;

// Six named slices + "Other" is the readable ceiling for a 180px ring.
const DONUT_TOP_N = 6;

const RANGES = { "1h": 3600, "24h": 86400, "7d": 604800, "30d": 2592000 };
const DEFAULT_RANGE_KEY = "24h";
const STORAGE_KEY = "quack.usage.timeframe";

const EMPTY_TOKENS_MSG = "no token data in this range (token metrics need quack v0.30+ to be emitting).";
const EMPTY_COST_MSG = "no cost data — cost needs a price table (providers.<p>.models.<model>) in quack's config, v0.30+.";
const EMPTY_LATENCY_MSG = `no data — the ${LATENCY.metric}_bucket histogram isn't present (check the OTel collector's Prometheus naming translation, or quack hasn't made a model call yet).`;

// Step heuristic: ~150 points per span, rounded up to a human-legible step.

const NICE_STEPS = [15, 30, 60, 300, 900, 1800, 3600, 7200, 21600, 43200, 86400, 172800, 604800];

function stepForSpan(spanSeconds) {
  let raw = Math.floor(spanSeconds / 150);
  if (raw < 15) raw = 15;
  for (const s of NICE_STEPS) {
    if (s >= raw) return s;
  }
  return raw;
}

// token_type spans panels, so it gets a FIXED color mapping. Other dimensions live in one card each
// and get a fresh discovery-order assigner; a 9th+ series folds to the neutral "other" color.

const MAX_PALETTE_SLOTS = 8;

function cssVar(name) {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}

const TOKEN_TYPE_SLOT = { input: 1, output: 3, reasoning: 4, cached: 6 };
function tokenTypeColor(t) {
  return cssVar(TOKEN_TYPE_SLOT[t] ? `--series-${TOKEN_TYPE_SLOT[t]}` : "--series-other");
}

function makeColorAssigner() {
  const slots = new Map();
  let next = 0;
  return function colorFor(value) {
    if (!slots.has(value)) {
      slots.set(value, next < MAX_PALETTE_SLOTS ? next++ : -1);
    }
    const slot = slots.get(value);
    return cssVar(slot < 0 ? "--series-other" : `--series-${slot + 1}`);
  };
}

// Latency percentiles get fixed colors: percentile identity is what must stay visually stable.
function latencyColor(quantileLabel) {
  if (quantileLabel === "p50") return cssVar("--series-1");
  if (quantileLabel === "p95") return cssVar("--series-2");
  return cssVar("--series-8"); // p99
}

// --- Prometheus fetch helpers, via this extension's own proxy only. ---

async function promQuery(query, time) {
  const params = new URLSearchParams({ query, time: String(time) });
  return promFetch("api/query?" + params.toString());
}

async function promQueryRange(query, start, end, step) {
  const params = new URLSearchParams({ query, start: String(start), end: String(end), step: String(step) });
  return promFetch("api/query_range?" + params.toString());
}

async function promFetch(path) {
  let resp;
  try {
    resp = await fetch(path, { headers: { Accept: "application/json" } });
  } catch (err) {
    return { networkError: String(err) };
  }
  let body;
  try {
    body = await resp.json();
  } catch (err) {
    return { networkError: "invalid response from usage API" };
  }
  return body;
}

// promResultOrStatus returns {ok:true, series} or {ok:false, empty, message}. "empty" means zero
// result vectors; multi-series panels (cache) must make their own presence check.
function promResultOrStatus(resp) {
  if (resp.networkError) {
    return { ok: false, empty: false, message: resp.networkError };
  }
  if (resp.status !== "success") {
    return { ok: false, empty: false, message: resp.error || "prometheus returned an error" };
  }
  const series = (resp.data && resp.data.result) || [];
  if (series.length === 0) {
    return { ok: false, empty: true, message: "" };
  }
  return { ok: true, series };
}

function dotMarker(cx, cy, color) {
  const dot = document.createElementNS(SVG_NS, "circle");
  dot.setAttribute("cx", cx.toFixed(1));
  dot.setAttribute("cy", cy.toFixed(1));
  dot.setAttribute("r", "3");
  dot.setAttribute("fill", color);
  return dot;
}

function firstLabel(metric, ...names) {
  for (const name of names) {
    if (metric[name]) return metric[name];
  }
  return "(unknown)";
}

// matrixSeriesPoints drops NaN samples: histogram_quantile emits "NaN" for an empty bucket,
// which is an absent point, not a zero.
function matrixSeriesPoints(item) {
  return item.values
    .map(([t, v]) => ({ t, v: Number(v) }))
    .filter((p) => !Number.isNaN(p.v));
}

function countNonZeroPoints(points) {
  let n = 0;
  for (const p of points) if (p.v > 0) n++;
  return n;
}

// shouldRenderBars decides columns-vs-line for a whole chart, never a mix. Only additive charts
// qualify, and the busiest series decides.
function shouldRenderBars(series, additive) {
  if (!additive) return false;
  let most = 0;
  for (const s of series) {
    const n = countNonZeroPoints(s.points);
    if (n > most) most = n;
  }
  return most > 0 && most < SPARSE_MAX_NONZERO;
}

// --- Formatting ---

function formatNumber(n) {
  if (n >= 1e9) return (n / 1e9).toFixed(2) + "B";
  if (n >= 1e6) return (n / 1e6).toFixed(2) + "M";
  if (n >= 1e3) return (n / 1e3).toFixed(2) + "K";
  return Math.round(n * 100) / 100 + "";
}

function formatMoney(n) {
  if (!Number.isFinite(n)) return "–";
  if (n >= 1000) return "$" + formatNumber(n);
  if (n < 1) return "$" + n.toFixed(4);
  return "$" + n.toFixed(2);
}

function formatPercent(n) {
  if (!Number.isFinite(n)) return "–";
  return n.toFixed(1) + "%";
}

function formatDuration(sec) {
  if (!Number.isFinite(sec)) return "–";
  if (sec < 1) return Math.round(sec * 1000) + "ms";
  if (sec < 120) return sec.toFixed(2) + "s";
  if (sec < 7200) return (sec / 60).toFixed(1) + "m";
  return (sec / 3600).toFixed(1) + "h";
}

function formatClock(t) {
  const d = new Date(t * 1000);
  return d.toLocaleString(undefined, { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

// --- Small DOM helpers ---

function clear(el) {
  while (el.firstChild) el.removeChild(el.firstChild);
}

function showEmpty(el, msg) {
  clear(el);
  const p = document.createElement("div");
  p.className = "chart-empty";
  p.textContent = msg;
  el.appendChild(p);
}

function showError(el, msg) {
  clear(el);
  const p = document.createElement("div");
  p.className = "chart-error";
  p.textContent = "error: " + msg;
  el.appendChild(p);
}

// tooltipRow builds a tooltip line from DOM nodes: `text` carries Prometheus label values, so it
// must never reach an HTML sink. `color` is ours.
function tooltipRow(color, text) {
  const row = document.createElement("div");
  const mark = document.createElement("span");
  mark.style.color = color;
  mark.textContent = "■";
  const label = document.createElement("span");
  label.textContent = " " + text;
  row.appendChild(mark);
  row.appendChild(label);
  return row;
}

function renderStat(el, label, value, title) {
  const tile = document.createElement("div");
  tile.className = "stat-tile";
  tile.textContent = value;
  if (title) tile.title = title;
  const sub = document.createElement("div");
  sub.className = "stat-sub";
  sub.textContent = label;
  el.appendChild(tile);
  el.appendChild(sub);
}

function panelEl(name) {
  return document.querySelector(`[data-panel="${name}"]`);
}

function donutEl(name) {
  return document.querySelector(`[data-donut="${name}"]`);
}

// Chart renderer: every panel shares the picker's [start, end] x-domain, a local legend toggle,
// and a hover crosshair + tooltip.

const SVG_NS = "http://www.w3.org/2000/svg";
const CHART_W = 600;
const CHART_H = 150;
const CHART_PAD = 8;

// renderSeriesChart(el, {series, start, end, step, mode, additive, hidden, emptyMsg, valueFormat,
// yMax, showLegend}) returns {redraw}. additive gates both stacking and the sparse->columns switch.
function renderSeriesChart(el, spec) {
  const valueFormat = spec.valueFormat || formatNumber;
  const nonEmpty = spec.series.filter((s) => s.points.length > 0);
  clear(el);
  if (nonEmpty.length === 0) {
    showEmpty(el, spec.emptyMsg || EMPTY_TOKENS_MSG);
    return { redraw() {} };
  }

  const hidden = spec.hidden || new Set();
  const wrap = document.createElement("div");
  wrap.className = "chart-wrap";
  el.appendChild(wrap);

  const tooltip = document.createElement("div");
  tooltip.className = "chart-tooltip";
  tooltip.style.display = "none";
  wrap.appendChild(tooltip);

  const axis = document.createElement("div");
  axis.className = "axis-labels";
  const axisLeft = document.createElement("span");
  const axisRight = document.createElement("span");
  axisRight.textContent = formatClock(spec.end);
  axis.appendChild(axisLeft);
  axis.appendChild(axisRight);

  const legendEl = document.createElement("div");
  legendEl.className = "chart-legend";

  const showLegend = spec.showLegend !== false && spec.series.length > 1;

  function draw() {
    while (wrap.firstChild !== tooltip) wrap.removeChild(wrap.firstChild);
    const visible = spec.series.filter((s) => !hidden.has(s.key));
    axisLeft.textContent = formatClock(spec.start);

    const pointMaps = new Map();
    const tickSet = new Set();
    for (const s of visible) {
      const m = new Map();
      for (const p of s.points) {
        m.set(p.t, p.v);
        tickSet.add(p.t);
      }
      pointMaps.set(s.key, m);
    }
    const ticks = Array.from(tickSet).sort((a, b) => a - b);

    const additive = spec.mode === "stacked-area" || spec.additive === true;
    const useBars = shouldRenderBars(visible, additive);
    const stackMarks = spec.mode === "stacked-area" || useBars;

    let yMax = spec.yMax;
    if (yMax === undefined) {
      if (stackMarks) {
        yMax = 0.0001;
        for (const t of ticks) {
          let sum = 0;
          for (const s of visible) sum += pointMaps.get(s.key).get(t) || 0;
          if (sum > yMax) yMax = sum;
        }
      } else {
        yMax = Math.max(0.0001, ...visible.flatMap((s) => s.points.map((p) => p.v)));
      }
    }

    const x = (t) => CHART_PAD + ((t - spec.start) / Math.max(1, spec.end - spec.start)) * (CHART_W - 2 * CHART_PAD);
    const y = (v) => CHART_H - CHART_PAD - (v / yMax) * (CHART_H - 2 * CHART_PAD);

    const svg = document.createElementNS(SVG_NS, "svg");
    svg.setAttribute("viewBox", `0 0 ${CHART_W} ${CHART_H}`);
    svg.setAttribute("class", "chart-svg");
    svg.setAttribute("preserveAspectRatio", "none");

    // Baseline (recessive, hairline).
    const baseline = document.createElementNS(SVG_NS, "line");
    baseline.setAttribute("x1", CHART_PAD);
    baseline.setAttribute("x2", CHART_W - CHART_PAD);
    baseline.setAttribute("y1", y(0));
    baseline.setAttribute("y2", y(0));
    baseline.setAttribute("stroke", "var(--gridline)");
    baseline.setAttribute("stroke-width", "1");
    svg.appendChild(baseline);

    if (useBars) {
      // One column per step that had traffic, stacked in series order; zero steps draw nothing.
      const span = Math.max(1, spec.end - spec.start);
      const stepSeconds = spec.step || (ticks.length > 1 ? ticks[1] - ticks[0] : span);
      const barW = Math.max(5, Math.min(28, (stepSeconds / span) * (CHART_W - 2 * CHART_PAD)));
      const bottoms = new Map();
      for (const s of visible) {
        const m = pointMaps.get(s.key);
        for (const t of ticks) {
          const v = m.get(t) || 0;
          if (v <= 0) continue;
          const base = bottoms.get(t) || 0;
          const rect = document.createElementNS(SVG_NS, "rect");
          const left = Math.max(0, Math.min(CHART_W - barW, x(t) - barW / 2));
          rect.setAttribute("x", left.toFixed(1));
          rect.setAttribute("y", y(base + v).toFixed(1));
          rect.setAttribute("width", barW.toFixed(1));
          rect.setAttribute("height", Math.max(1, y(base) - y(base + v)).toFixed(1));
          rect.setAttribute("fill", s.color);
          svg.appendChild(rect);
          bottoms.set(t, base + v);
        }
      }
    } else if (spec.mode === "stacked-area") {
      let cumBottom = ticks.map(() => 0);
      for (const s of visible) {
        const m = pointMaps.get(s.key);
        const top = ticks.map((t, i) => cumBottom[i] + (m.get(t) || 0));
        const bandPointsTop = ticks.map((t, i) => `${x(t).toFixed(1)},${y(top[i]).toFixed(1)}`);
        const bandPointsBottom = ticks
          .map((t, i) => `${x(t).toFixed(1)},${y(cumBottom[i]).toFixed(1)}`)
          .reverse();
        const band = document.createElementNS(SVG_NS, "polygon");
        band.setAttribute("points", bandPointsTop.concat(bandPointsBottom).join(" "));
        band.setAttribute("fill", s.color);
        band.setAttribute("fill-opacity", "0.55");
        band.setAttribute("stroke", "none");
        svg.appendChild(band);

        const line = document.createElementNS(SVG_NS, "polyline");
        line.setAttribute("points", bandPointsTop.join(" "));
        line.setAttribute("fill", "none");
        line.setAttribute("stroke", s.color);
        line.setAttribute("stroke-width", "2");
        svg.appendChild(line);
        // A 1-2 tick series draws no visible band/segment - mark the points.
        if (ticks.length <= 2) for (let i = 0; i < ticks.length; i++) svg.appendChild(dotMarker(x(ticks[i]), y(top[i]), s.color));

        cumBottom = top;
      }
    } else {
      for (const s of visible) {
        const pts = s.points.map((p) => `${x(p.t).toFixed(1)},${y(p.v).toFixed(1)}`).join(" ");
        const line = document.createElementNS(SVG_NS, "polyline");
        line.setAttribute("points", pts);
        line.setAttribute("fill", "none");
        line.setAttribute("stroke", s.color);
        line.setAttribute("stroke-width", "2");
        svg.appendChild(line);
        // A 1-2 point series has no visible segment: mark the points instead.
        if (s.points.length <= 2) for (const p of s.points) svg.appendChild(dotMarker(x(p.t), y(p.v), s.color));
      }
    }

    // Hover crosshair + tooltip.
    const guide = document.createElementNS(SVG_NS, "line");
    guide.setAttribute("y1", CHART_PAD);
    guide.setAttribute("y2", CHART_H - CHART_PAD);
    guide.setAttribute("stroke", "var(--baseline)");
    guide.setAttribute("stroke-width", "1");
    guide.style.display = "none";
    svg.appendChild(guide);

    const capture = document.createElementNS(SVG_NS, "rect");
    capture.setAttribute("x", "0");
    capture.setAttribute("y", "0");
    capture.setAttribute("width", String(CHART_W));
    capture.setAttribute("height", String(CHART_H));
    capture.setAttribute("fill", "transparent");
    svg.appendChild(capture);

    if (ticks.length > 0) {
      capture.addEventListener("mousemove", (evt) => {
        const rect = svg.getBoundingClientRect();
        const relX = ((evt.clientX - rect.left) / rect.width) * CHART_W;
        const t = spec.start + ((relX - CHART_PAD) / (CHART_W - 2 * CHART_PAD)) * (spec.end - spec.start);
        let nearest = ticks[0];
        let nearestDist = Math.abs(t - nearest);
        for (const tick of ticks) {
          const d = Math.abs(t - tick);
          if (d < nearestDist) {
            nearest = tick;
            nearestDist = d;
          }
        }
        guide.setAttribute("x1", x(nearest).toFixed(1));
        guide.setAttribute("x2", x(nearest).toFixed(1));
        guide.style.display = "";

        clear(tooltip);
        const head = document.createElement("strong");
        head.textContent = formatClock(nearest);
        tooltip.appendChild(head);
        for (const s of visible) {
          const v = pointMaps.get(s.key).get(nearest);
          tooltip.appendChild(tooltipRow(s.color, `${s.label}: ${v === undefined ? "–" : valueFormat(v)}`));
        }
        tooltip.style.display = "";
        tooltip.style.left = Math.min(evt.clientX - rect.left + 10, rect.width - 160) + "px";
        tooltip.style.top = "0px";
      });
      capture.addEventListener("mouseleave", () => {
        guide.style.display = "none";
        tooltip.style.display = "none";
      });
    }

    wrap.insertBefore(svg, tooltip);
    wrap.insertBefore(axis, tooltip);
  }

  draw();

  if (showLegend) {
    clear(legendEl);
    for (const s of spec.series) {
      const btn = document.createElement("button");
      btn.type = "button";
      btn.className = "legend-item";
      btn.setAttribute("aria-pressed", String(!hidden.has(s.key)));
      const swatch = document.createElement("span");
      swatch.className = "legend-swatch";
      swatch.style.background = s.color;
      const label = document.createElement("span");
      label.textContent = s.label;
      btn.appendChild(swatch);
      btn.appendChild(label);
      btn.addEventListener("click", () => {
        if (hidden.has(s.key)) {
          hidden.delete(s.key);
          btn.setAttribute("aria-pressed", "true");
        } else {
          hidden.add(s.key);
          btn.setAttribute("aria-pressed", "false");
        }
        draw();
      });
      legendEl.appendChild(btn);
    }
    el.appendChild(legendEl);
  }

  return { redraw: draw };
}

// Donut: each label's share of the window. Arcs are stroke-dasharray on circles so a 100% slice
// is a full circle, not the degenerate arc a 360-degree path A command draws.

const DONUT_SIZE = 180;
const DONUT_R = 66;
const DONUT_STROKE = 26;
const DONUT_GAP = 1.5; // units of circumference shaved off each arc's end

// renderDonut(el, {slices, hidden, onToggle, caption, emptyMsg}); slices arrive ranked and colored.
// Hidden slices dim but keep their share: re-basing on a toggle reads as data changing.
function renderDonut(el, spec) {
  clear(el);
  const hidden = spec.hidden || new Set();
  const slices = spec.slices.filter((s) => Number.isFinite(s.value) && s.value > 0);
  const total = slices.reduce((a, s) => a + s.value, 0);
  if (slices.length === 0 || total <= 0) {
    showEmpty(el, spec.emptyMsg || EMPTY_TOKENS_MSG);
    return;
  }

  const wrap = document.createElement("div");
  wrap.className = "chart-wrap donut-wrap";
  el.appendChild(wrap);

  const tooltip = document.createElement("div");
  tooltip.className = "chart-tooltip";
  tooltip.style.display = "none";
  wrap.appendChild(tooltip);

  const svg = document.createElementNS(SVG_NS, "svg");
  svg.setAttribute("viewBox", `0 0 ${DONUT_SIZE} ${DONUT_SIZE}`);
  svg.setAttribute("class", "donut-svg");
  const mid = DONUT_SIZE / 2;

  const track = document.createElementNS(SVG_NS, "circle");
  track.setAttribute("cx", String(mid));
  track.setAttribute("cy", String(mid));
  track.setAttribute("r", String(DONUT_R));
  track.setAttribute("fill", "none");
  track.setAttribute("stroke", "var(--gridline)");
  track.setAttribute("stroke-width", String(DONUT_STROKE));
  svg.appendChild(track);

  const ring = document.createElementNS(SVG_NS, "g");
  ring.setAttribute("transform", `rotate(-90 ${mid} ${mid})`);
  svg.appendChild(ring);

  const circumference = 2 * Math.PI * DONUT_R;
  const arcs = new Map();
  let acc = 0;
  for (const s of slices) {
    const frac = s.value / total;
    const arc = document.createElementNS(SVG_NS, "circle");
    arc.setAttribute("class", "donut-arc");
    arc.setAttribute("cx", String(mid));
    arc.setAttribute("cy", String(mid));
    arc.setAttribute("r", String(DONUT_R));
    arc.setAttribute("fill", "none");
    arc.setAttribute("stroke", s.color);
    arc.setAttribute("stroke-width", String(DONUT_STROKE));
    const len = slices.length > 1 ? Math.max(1, frac * circumference - DONUT_GAP) : circumference;
    arc.setAttribute("stroke-dasharray", `${len.toFixed(2)} ${(circumference - len).toFixed(2)}`);
    arc.setAttribute("stroke-dashoffset", (-acc * circumference).toFixed(2));
    ring.appendChild(arc);
    arcs.set(s.key, arc);
    acc += frac;

    arc.addEventListener("mousemove", (evt) => {
      paint(s.key);
      const rect = wrap.getBoundingClientRect();
      clear(tooltip);
      tooltip.appendChild(tooltipRow(s.color, s.label));
      const sub = document.createElement("div");
      sub.textContent = `${formatNumber(s.value)} (${formatPercent((s.value / total) * 100)})`;
      tooltip.appendChild(sub);
      tooltip.style.display = "";
      tooltip.style.left = Math.max(0, Math.min(evt.clientX - rect.left + 10, rect.width - 140)) + "px";
      tooltip.style.top = "0px";
    });
    arc.addEventListener("mouseleave", () => {
      paint(null);
      tooltip.style.display = "none";
    });
  }

  const totalText = document.createElementNS(SVG_NS, "text");
  totalText.setAttribute("class", "donut-total");
  totalText.setAttribute("x", String(mid));
  totalText.setAttribute("y", String(mid + 2));
  totalText.setAttribute("text-anchor", "middle");
  totalText.textContent = formatNumber(total);
  svg.appendChild(totalText);

  const capText = document.createElementNS(SVG_NS, "text");
  capText.setAttribute("class", "donut-caption");
  capText.setAttribute("x", String(mid));
  capText.setAttribute("y", String(mid + 20));
  capText.setAttribute("text-anchor", "middle");
  capText.textContent = spec.caption || "total";
  svg.appendChild(capText);

  wrap.insertBefore(svg, tooltip);

  const legendEl = document.createElement("div");
  legendEl.className = "donut-legend";
  const rows = new Map();
  for (const s of slices) {
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "donut-legend-item";
    btn.title = `${s.label}: ${formatNumber(s.value)}`;
    const swatch = document.createElement("span");
    swatch.className = "legend-swatch";
    swatch.style.background = s.color;
    const label = document.createElement("span");
    label.className = "donut-legend-label";
    label.textContent = s.label;
    const value = document.createElement("span");
    value.className = "donut-legend-value";
    value.textContent = formatNumber(s.value);
    const pct = document.createElement("span");
    pct.className = "donut-legend-pct";
    pct.textContent = formatPercent((s.value / total) * 100);
    btn.append(swatch, label, value, pct);
    btn.addEventListener("mouseenter", () => paint(s.key));
    btn.addEventListener("mouseleave", () => paint(null));
    btn.addEventListener("click", () => {
      if (hidden.has(s.key)) hidden.delete(s.key);
      else hidden.add(s.key);
      paint(null);
      if (spec.onToggle) spec.onToggle(s.key);
    });
    rows.set(s.key, btn);
    legendEl.appendChild(btn);
  }
  el.appendChild(legendEl);

  function paint(focusKey) {
    for (const s of slices) {
      const isHidden = hidden.has(s.key);
      const arc = arcs.get(s.key);
      let opacity = 1;
      if (isHidden) opacity = 0.15;
      else if (focusKey && focusKey !== s.key) opacity = 0.3;
      arc.setAttribute("opacity", String(opacity));
      arc.setAttribute("stroke-width", String(!isHidden && focusKey === s.key ? DONUT_STROKE + 5 : DONUT_STROKE));
      rows.get(s.key).setAttribute("aria-pressed", String(!isHidden));
    }
  }
  paint(null);
}

// Query builders

function tokenTypeTotalsQuery(rangeSeconds) {
  return `sum by (${METRICS.labels.tokenType}, ${METRICS.labels.tokenTypeFallback}) (increase(${METRICS.tokenUsage}[${rangeSeconds}s]))`;
}

function tokenTypeSeriesQuery(stepSeconds) {
  return `sum by (${METRICS.labels.tokenType}, ${METRICS.labels.tokenTypeFallback}) (increase(${METRICS.tokenUsage}[${stepSeconds}s]))`;
}

function dimSeriesQuery(dimLabel, stepSeconds) {
  return `sum by (${dimLabel}) (increase(${METRICS.tokenUsage}[${stepSeconds}s]))`;
}

function modelSeriesQuery(stepSeconds) {
  return `sum by (${METRICS.labels.model}, ${METRICS.labels.modelFallback}) (increase(${METRICS.tokenUsage}[${stepSeconds}s]))`;
}

// One instant query crosses a dimension with token_type, so a label's cached and input volumes
// can't drift against each other.
function cacheByModelQuery(rangeSeconds) {
  return `sum by (${METRICS.labels.model}, ${METRICS.labels.modelFallback}, ${METRICS.labels.tokenType}, ${METRICS.labels.tokenTypeFallback}) (increase(${METRICS.tokenUsage}[${rangeSeconds}s]))`;
}

function cacheByAgentQuery(rangeSeconds) {
  return `sum by (${METRICS.labels.agent}, ${METRICS.labels.tokenType}, ${METRICS.labels.tokenTypeFallback}) (increase(${METRICS.tokenUsage}[${rangeSeconds}s]))`;
}

function costTotalQuery(rangeSeconds) {
  return `sum(increase({__name__=~"${METRICS.costNameRegex}"}[${rangeSeconds}s]))`;
}

function costSeriesQuery(stepSeconds) {
  return `sum(increase({__name__=~"${METRICS.costNameRegex}"}[${stepSeconds}s]))`;
}

function callCountQuery(rangeSeconds) {
  return `sum(increase(${LATENCY.metric}_count[${rangeSeconds}s]))`;
}

function latencyPresenceQuery() {
  return `count(${LATENCY.metric}_bucket)`;
}

function latencyInstantQuantileQuery(q, rangeSeconds) {
  return `histogram_quantile(${q}, sum by (le) (rate(${LATENCY.metric}_bucket[${rangeSeconds}s])))`;
}

function latencySeriesQuantileQuery(q, stepSeconds) {
  return `histogram_quantile(${q}, sum by (le) (rate(${LATENCY.metric}_bucket[${stepSeconds}s])))`;
}

// Panel loaders: each queries Prometheus and renders into its own mount point.

function topKWithOther(series, k) {
  // Keeps the top k by total and folds the rest into one "Other" series (isOther gets the neutral
  // color, never a generated hue).
  const sorted = series.slice().sort((a, b) => b.total - a.total);
  if (sorted.length <= k) return sorted.map((s) => ({ ...s, isOther: false }));
  const kept = sorted.slice(0, k).map((s) => ({ ...s, isOther: false }));
  const rest = sorted.slice(k);
  const otherPoints = new Map();
  for (const s of rest) {
    for (const p of s.points) {
      otherPoints.set(p.t, (otherPoints.get(p.t) || 0) + p.v);
    }
  }
  kept.push({
    label: `Other (${rest.length})`,
    isOther: true,
    total: rest.reduce((a, s) => a + s.total, 0),
    points: Array.from(otherPoints.entries())
      .map(([t, v]) => ({ t, v }))
      .sort((a, b) => a.t - b.t),
  });
  return kept;
}

// dimQuery feeds both the range query (window = step) and the instant query (window = range),
// so a card's two halves never measure different things.
function dimQuery(dimLabel, windowSeconds) {
  return dimLabel === "__model__" ? modelSeriesQuery(windowSeconds) : dimSeriesQuery(dimLabel, windowSeconds);
}

function dimValue(metric, dimLabel) {
  if (dimLabel === "__model__") return firstLabel(metric, METRICS.labels.model, METRICS.labels.modelFallback);
  return metric[dimLabel] || "(unknown)";
}

// loadDimPanel fills one dimension card from ONE ranking of the instant totals, colored once,
// so a label has one color in the donut and the series and one legend drives both.
async function loadDimPanel(name, dimLabel, win) {
  const seriesMount = panelEl(name);
  const donutMount = donutEl(name);

  const [rangeResp, totalResp] = await Promise.all([
    promQueryRange(dimQuery(dimLabel, win.step), win.start, win.end, win.step),
    promQuery(dimQuery(dimLabel, win.rangeSeconds), win.now),
  ]);
  const rangeStatus = promResultOrStatus(rangeResp);
  const totalStatus = promResultOrStatus(totalResp);
  const rangeFailed = !rangeStatus.ok && !rangeStatus.empty;
  const totalFailed = !totalStatus.ok && !totalStatus.empty;
  if (rangeFailed) showError(seriesMount, rangeStatus.message);
  if (totalFailed) showError(donutMount, totalStatus.message);
  if (rangeFailed && totalFailed) return;

  // Two series can resolve to the same display label (the model dimension
  // groups by both the translated and unqualified name) - fold them.
  const pointsByLabel = new Map();
  if (rangeStatus.ok) {
    for (const s of rangeStatus.series) {
      const label = dimValue(s.metric, dimLabel);
      if (!pointsByLabel.has(label)) pointsByLabel.set(label, new Map());
      const m = pointsByLabel.get(label);
      for (const p of matrixSeriesPoints(s)) m.set(p.t, (m.get(p.t) || 0) + p.v);
    }
  }
  const pointsOf = (label) =>
    Array.from((pointsByLabel.get(label) || new Map()).entries())
      .map(([t, v]) => ({ t, v }))
      .sort((a, b) => a.t - b.t);

  const totals = new Map();
  if (totalStatus.ok) {
    for (const s of totalStatus.series) {
      const label = dimValue(s.metric, dimLabel);
      totals.set(label, (totals.get(label) || 0) + Number(s.value[1]));
    }
  } else {
    // Instant query empty/failed: rank off the range's own sums so the series still renders; the
    // donut stays empty rather than show a total it didn't measure.
    for (const [label, m] of pointsByLabel) {
      totals.set(label, Array.from(m.values()).reduce((a, v) => a + v, 0));
    }
  }

  const entries = Array.from(totals, ([label, total]) => ({ label, total, points: pointsOf(label) }));
  const ranked = topKWithOther(entries, DONUT_TOP_N);
  const colorFor = makeColorAssigner();
  const colored = ranked.map((s) => ({
    key: s.label,
    label: s.label,
    total: s.total,
    points: s.points,
    color: s.isOther ? cssVar("--series-other") : colorFor(s.label),
  }));

  // One hidden-set for the card: the donut's legend is the only legend, so
  // clicking a slice hides that series in the plot beside it too.
  const hidden = new Set();
  const chart = rangeFailed
    ? { redraw() {} }
    : renderSeriesChart(seriesMount, {
        series: colored.map((s) => ({ key: s.key, label: s.label, color: s.color, points: s.points })),
        start: win.start,
        end: win.end,
        step: win.step,
        mode: "lines",
        additive: true,
        hidden,
        showLegend: false,
        emptyMsg: EMPTY_TOKENS_MSG,
      });
  if (totalFailed) return;
  renderDonut(donutMount, {
    slices: totalStatus.ok ? colored.map((s) => ({ key: s.key, label: s.label, value: s.total, color: s.color })) : [],
    caption: "tokens",
    hidden,
    onToggle: () => chart.redraw(),
    emptyMsg: EMPTY_TOKENS_MSG,
  });
}

async function loadHeadlineTokens(start, end, step) {
  const el = panelEl("tokens-headline");
  const resp = await promQueryRange(tokenTypeSeriesQuery(step), start, end, step);
  const status = promResultOrStatus(resp);
  if (!status.ok) {
    if (status.empty) return showEmpty(el, EMPTY_TOKENS_MSG);
    return showError(el, status.message);
  }
  const byType = new Map();
  for (const s of status.series) {
    const t = s.metric[METRICS.labels.tokenType] || s.metric[METRICS.labels.tokenTypeFallback];
    if (t) byType.set(t, matrixSeriesPoints(s));
  }
  const series = TOKEN_TYPES.filter((t) => byType.has(t)).map((t) => ({
    key: t,
    label: t,
    color: tokenTypeColor(t),
    points: byType.get(t),
  }));
  renderSeriesChart(el, { series, start, end, step, mode: "stacked-area", emptyMsg: EMPTY_TOKENS_MSG });
  return byType;
}

// cacheRateFor returns null, not 0, for zero prompt traffic so an idle model/agent is excluded
// rather than rendered as a fake "0%" row.
function cacheRateFor(cached, input) {
  const volume = cached + input;
  if (volume <= 0) return null;
  return (cached / volume) * 100;
}

// aggregateCacheByDim folds per (label, token_type) series into {cached, input, output} per label.
// output only tells embeddings apart from a real 0% cache rate; it never enters the rate math.
function aggregateCacheByDim(series, dimLabel) {
  const totals = new Map();
  for (const s of series) {
    const t = s.metric[METRICS.labels.tokenType] || s.metric[METRICS.labels.tokenTypeFallback];
    if (t !== METRICS.tokenTypeCached && t !== METRICS.tokenTypeInput && t !== METRICS.tokenTypeOutput) continue;
    const label = dimValue(s.metric, dimLabel);
    const entry = totals.get(label) || { cached: 0, input: 0, output: 0 };
    entry[t] += Number(s.value[1]);
    totals.set(label, entry);
  }
  return totals;
}

// cacheRowsFromTotals drops zero-traffic rows and ranks by volume. Prompt traffic with zero output
// is embeddings, which have no cache concept: rate null (n/a), not dropped.
function cacheRowsFromTotals(totals) {
  const rows = [];
  for (const [label, { cached, input, output }] of totals) {
    if (cached <= 0 && input <= 0) continue;
    if (cached === 0 && output === 0) {
      rows.push({ label, cached, input, volume: input, rate: null });
      continue;
    }
    const rate = cacheRateFor(cached, input);
    if (rate === null) continue;
    rows.push({ label, cached, input, volume: cached + input, rate });
  }
  rows.sort((a, b) => b.volume - a.volume);
  return rows;
}

function renderCacheOverall(el, cached, input) {
  clear(el);
  const rate = cacheRateFor(cached, input);
  if (rate === null) {
    showEmpty(el, EMPTY_TOKENS_MSG);
    return;
  }
  const tile = document.createElement("div");
  tile.className = "stat-tile";
  tile.textContent = formatPercent(rate);
  const sub = document.createElement("div");
  sub.className = "stat-sub";
  sub.textContent = "cache rate, overall";
  const detail = document.createElement("div");
  detail.className = "cache-detail";
  detail.textContent = `${formatNumber(cached)} cached of ${formatNumber(cached + input)} prompt`;
  el.append(tile, sub, detail);
}

function renderCacheRows(el, rows) {
  clear(el);
  if (rows.length === 0) {
    showEmpty(el, EMPTY_TOKENS_MSG);
    return;
  }
  for (const row of rows) {
    const r = document.createElement("div");
    r.className = "cache-row";
    const label = document.createElement("span");
    label.className = "cache-row-label";
    label.textContent = row.label;
    label.title = row.label;
    const rate = document.createElement("span");
    rate.className = "cache-row-rate";
    // row.rate is null for embeddings-shaped traffic: "n/a", not 0.0%.
    rate.textContent = row.rate === null ? "n/a" : formatPercent(row.rate);
    const vol = document.createElement("span");
    vol.className = "cache-row-volume";
    vol.textContent = formatNumber(row.volume) + " prompt";
    r.append(label, rate, vol);
    el.appendChild(r);
  }
}

// loadCachePanel renders the cache section and savings tile. Empty is ONE decision for the whole
// section: no prompt traffic means the empty message everywhere, never a 0%.
async function loadCachePanel(win, instantTotals) {
  const overallEl = panelEl("cache-overall");
  const modelEl = panelEl("cache-by-model");
  const agentEl = panelEl("cache-by-agent");
  const savingsEl = panelEl("cache-savings");

  const cachedTotal = instantTotals.tokens.cached || 0;
  const inputTotal = instantTotals.tokens.input || 0;

  if (cacheRateFor(cachedTotal, inputTotal) === null) {
    showEmpty(overallEl, EMPTY_TOKENS_MSG);
    showEmpty(modelEl, EMPTY_TOKENS_MSG);
    showEmpty(agentEl, EMPTY_TOKENS_MSG);
  } else {
    renderCacheOverall(overallEl, cachedTotal, inputTotal);

    const [modelResp, agentResp] = await Promise.all([
      promQuery(cacheByModelQuery(win.rangeSeconds), win.now),
      promQuery(cacheByAgentQuery(win.rangeSeconds), win.now),
    ]);
    for (const [el, resp, dimLabel] of [
      [modelEl, modelResp, "__model__"],
      [agentEl, agentResp, METRICS.labels.agent],
    ]) {
      const status = promResultOrStatus(resp);
      if (status.ok) {
        renderCacheRows(el, cacheRowsFromTotals(aggregateCacheByDim(status.series, dimLabel)));
      } else if (status.empty) {
        showEmpty(el, EMPTY_TOKENS_MSG);
      } else {
        showError(el, status.message);
      }
    }
  }

  // Cache savings (est.) = cached_tokens * (cost / input_tokens): per-token price isn't queryable.
  // Empty only when the cost series is absent, independent of the rate panels.
  const formula = "est. = cached_tokens × (cost ÷ input_tokens)";
  if (!instantTotals.cost.ok) {
    if (instantTotals.cost.empty) {
      showEmpty(savingsEl, EMPTY_COST_MSG);
    } else {
      showError(savingsEl, instantTotals.cost.message);
    }
    return;
  }
  if (inputTotal <= 0) {
    showEmpty(savingsEl, EMPTY_TOKENS_MSG);
    return;
  }
  const savings = cachedTotal * (instantTotals.cost.value / inputTotal);
  clear(savingsEl);
  renderStat(savingsEl, "cache savings (est.)", formatMoney(savings), formula);
}

async function loadCostPanel(start, end, step, instantCost) {
  const perStepEl = panelEl("cost");
  const cumulativeEl = panelEl("cost-cumulative");

  const resp = await promQueryRange(costSeriesQuery(step), start, end, step);
  const status = promResultOrStatus(resp);
  if (!status.ok) {
    if (status.empty) {
      showEmpty(perStepEl, EMPTY_COST_MSG);
      showEmpty(cumulativeEl, EMPTY_COST_MSG);
    } else {
      showError(perStepEl, status.message);
      showError(cumulativeEl, status.message);
    }
    return;
  }

  const points = matrixSeriesPoints(status.series[0]);
  renderSeriesChart(perStepEl, {
    series: [{ key: "cost", label: "cost", color: cssVar("--series-1"), points }],
    start,
    end,
    step,
    mode: "stacked-area",
    valueFormat: formatMoney,
    showLegend: false,
    emptyMsg: EMPTY_COST_MSG,
  });

  let running = 0;
  const cumPoints = points.map((p) => {
    running += p.v;
    return { t: p.t, v: running };
  });
  renderSeriesChart(cumulativeEl, {
    series: [{ key: "cost-cumulative", label: "cumulative cost", color: cssVar("--series-2"), points: cumPoints }],
    start,
    end,
    step,
    mode: "lines",
    valueFormat: formatMoney,
    showLegend: false,
    emptyMsg: EMPTY_COST_MSG,
  });

  const totalTile = document.createElement("div");
  totalTile.className = "chart-sub";
  totalTile.textContent = instantCost.ok ? `total for range: ${formatMoney(instantCost.value)}` : "";
  if (instantCost.ok) cumulativeEl.appendChild(totalTile);
}

async function loadLatencyPanel(start, end, step, latencyAvailable) {
  const el = panelEl("latency");
  if (!latencyAvailable) {
    showEmpty(el, EMPTY_LATENCY_MSG);
    return;
  }

  const quantiles = [
    { q: "0.5", key: "p50", label: "p50" },
    { q: "0.95", key: "p95", label: "p95" },
    { q: "0.99", key: "p99", label: "p99" },
  ];
  const responses = await Promise.all(
    quantiles.map((q) => promQueryRange(latencySeriesQuantileQuery(q.q, step), start, end, step))
  );

  const series = [];
  for (let i = 0; i < quantiles.length; i++) {
    const status = promResultOrStatus(responses[i]);
    if (!status.ok) continue;
    const points = matrixSeriesPoints(status.series[0]);
    if (points.length === 0) continue;
    series.push({ key: quantiles[i].key, label: quantiles[i].label, color: latencyColor(quantiles[i].key), points });
  }

  renderSeriesChart(el, {
    series,
    start,
    end,
    step,
    mode: "lines",
    valueFormat: formatDuration,
    emptyMsg: EMPTY_LATENCY_MSG,
  });
}

// KPI row: sticky instant sums above every chart, computed once per refresh.

function setKPI(name, value, title) {
  const tile = document.querySelector(`[data-kpi="${name}"] .kpi-value`);
  if (!tile) return;
  tile.textContent = value;
  if (title) tile.title = title;
}

async function loadKPIRow(rangeSeconds, now, latencyAvailable, tokenTotals, costTotal) {
  const totalTokens = TOKEN_TYPES.reduce((a, t) => a + (tokenTotals[t] || 0), 0);
  setKPI("tokens", totalTokens > 0 ? formatNumber(totalTokens) : "–");
  setKPI("cost", costTotal.ok ? formatMoney(costTotal.value) : "–", costTotal.empty ? EMPTY_COST_MSG : undefined);

  if (!latencyAvailable) {
    setKPI("calls", "–", EMPTY_LATENCY_MSG);
    setKPI("latency-p95", "–", EMPTY_LATENCY_MSG);
    return;
  }
  const [callsResp, p95Resp] = await Promise.all([
    promQuery(callCountQuery(rangeSeconds), now),
    promQuery(latencyInstantQuantileQuery("0.95", rangeSeconds), now),
  ]);
  const callsStatus = promResultOrStatus(callsResp);
  const p95Status = promResultOrStatus(p95Resp);
  // A call count is an integer; increase() extrapolates fractions.
  setKPI("calls", callsStatus.ok ? formatNumber(Math.round(Number(callsStatus.series[0].value[1]))) : "–");
  setKPI("latency-p95", p95Status.ok ? formatDuration(Number(p95Status.series[0].value[1])) : "–");
}

// --- instant totals shared by the KPI row and the cache-savings tile ---

async function fetchInstantTotals(rangeSeconds, now) {
  const [tokenResp, costResp] = await Promise.all([
    promQuery(tokenTypeTotalsQuery(rangeSeconds), now),
    promQuery(costTotalQuery(rangeSeconds), now),
  ]);
  const tokenStatus = promResultOrStatus(tokenResp);
  const tokens = {};
  if (tokenStatus.ok) {
    for (const s of tokenStatus.series) {
      const t = s.metric[METRICS.labels.tokenType] || s.metric[METRICS.labels.tokenTypeFallback];
      if (t) tokens[t] = Number(s.value[1]);
    }
  }
  const costStatus = promResultOrStatus(costResp);
  const cost = costStatus.ok
    ? { ok: true, value: Number(costStatus.series[0].value[1]) }
    : { ok: false, empty: costStatus.empty, message: costStatus.message };
  return { tokens, cost };
}

// Timeframe state: presets + custom range, persisted to localStorage; drives every panel's domain.

function loadStoredTimeframe() {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return null;
    const parsed = JSON.parse(raw);
    if (parsed.mode === "preset" && RANGES[parsed.presetKey]) return parsed;
    if (parsed.mode === "custom" && typeof parsed.customFrom === "string" && typeof parsed.customTo === "string") {
      return parsed;
    }
  } catch (err) {
    // corrupt/old localStorage value - fall through to the default.
  }
  return null;
}

function saveTimeframe(tf) {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(tf));
  } catch (err) {
    // localStorage unavailable (private browsing): persistence is a nicety.
  }
}

// resolveRange turns a timeframe descriptor into {start, end, step,
// rangeSeconds}, in unix seconds.
function resolveRange(tf) {
  if (tf.mode === "custom") {
    const start = Math.floor(new Date(tf.customFrom).getTime() / 1000);
    const end = Math.floor(new Date(tf.customTo).getTime() / 1000);
    const rangeSeconds = Math.max(1, end - start);
    return { start, end, rangeSeconds, step: stepForSpan(rangeSeconds) };
  }
  const rangeSeconds = RANGES[tf.presetKey] || RANGES[DEFAULT_RANGE_KEY];
  const end = Math.floor(Date.now() / 1000);
  const start = end - rangeSeconds;
  return { start, end, rangeSeconds, step: stepForSpan(rangeSeconds) };
}

async function refreshAll(tf) {
  saveTimeframe(tf);
  const { start, end, rangeSeconds, step } = resolveRange(tf);

  const now = Math.floor(Date.now() / 1000);
  const [instantTotals, presenceResp] = await Promise.all([
    fetchInstantTotals(rangeSeconds, now),
    promQuery(latencyPresenceQuery(), now),
  ]);
  const presenceStatus = promResultOrStatus(presenceResp);
  const latencyAvailable = presenceStatus.ok && Number(presenceStatus.series[0].value[1]) > 0;

  const win = { start, end, step, rangeSeconds, now };
  await Promise.all([
    loadHeadlineTokens(start, end, step),
    loadDimPanel("tokens-model", "__model__", win),
    loadDimPanel("tokens-source", METRICS.labels.source, win),
    loadDimPanel("tokens-agent", METRICS.labels.agent, win),
    loadDimPanel("tokens-user", METRICS.labels.user, win),
    loadCachePanel(win, instantTotals),
    loadCostPanel(start, end, step, instantTotals.cost),
    loadLatencyPanel(start, end, step, latencyAvailable),
    loadKPIRow(rangeSeconds, now, latencyAvailable, instantTotals.tokens, instantTotals.cost),
  ].map((p) => p.catch((err) => console.error("usage: panel load failed", err))));
}

// --- Timeframe UI wiring ---

function toDatetimeLocalValue(epochSeconds) {
  const d = new Date(epochSeconds * 1000);
  const pad = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

function initTimeframeUI() {
  const cfg = window.__USAGE_CONFIG__ || {};
  const defaultKey = Object.keys(RANGES).find((k) => RANGES[k] === cfg.defaultRangeSeconds) || DEFAULT_RANGE_KEY;

  let tf = loadStoredTimeframe() || { mode: "preset", presetKey: defaultKey };

  // Presets carry data-range; "Custom…" has none and only reveals the date row until Apply.
  const presetButtons = document.querySelectorAll("#range-select button[data-range]");
  const customBtn = document.getElementById("range-custom");
  const customRow = document.getElementById("custom-range");
  const fromInput = document.getElementById("range-from");
  const toInput = document.getElementById("range-to");
  const applyBtn = document.getElementById("range-apply");
  const errorEl = document.getElementById("range-error");

  function syncPressedState() {
    presetButtons.forEach((b) => {
      b.setAttribute("aria-pressed", String(tf.mode === "preset" && b.dataset.range === tf.presetKey));
    });
    customBtn.setAttribute("aria-pressed", String(tf.mode === "custom"));
  }

  function showCustomRow(show) {
    customRow.hidden = !show;
    customBtn.setAttribute("aria-expanded", String(show));
  }

  function seedCustomInputs() {
    const { start, end } = resolveRange(tf);
    fromInput.value = toDatetimeLocalValue(start);
    toInput.value = toDatetimeLocalValue(end);
  }

  presetButtons.forEach((btn) => {
    btn.addEventListener("click", () => {
      tf = { mode: "preset", presetKey: btn.dataset.range };
      errorEl.textContent = "";
      syncPressedState();
      seedCustomInputs();
      showCustomRow(false);
      refreshAll(tf);
    });
  });

  customBtn.addEventListener("click", () => {
    const opening = customRow.hidden;
    if (opening && tf.mode !== "custom") seedCustomInputs();
    showCustomRow(opening);
    if (opening) fromInput.focus();
  });

  applyBtn.addEventListener("click", () => {
    if (!fromInput.value || !toInput.value) {
      errorEl.textContent = "pick both a from and to date/time.";
      return;
    }
    const fromMs = new Date(fromInput.value).getTime();
    const toMs = new Date(toInput.value).getTime();
    if (Number.isNaN(fromMs) || Number.isNaN(toMs)) {
      errorEl.textContent = "invalid date/time.";
      return;
    }
    if (fromMs >= toMs) {
      errorEl.textContent = "\"from\" must be before \"to\".";
      return;
    }
    errorEl.textContent = "";
    tf = { mode: "custom", customFrom: fromInput.value, customTo: toInput.value };
    syncPressedState();
    refreshAll(tf);
  });

  syncPressedState();
  seedCustomInputs();
  showCustomRow(tf.mode === "custom");
  return tf;
}

document.addEventListener("DOMContentLoaded", () => {
  const tf = initTimeframeUI();
  refreshAll(tf);
});
