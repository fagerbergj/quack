package usage

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestExtension(t *testing.T) *extension {
	t.Helper()
	extVal, err := factory(newTestHost(), []byte("prometheus_url: http://prom:9090\ntempo_url: http://tempo:3200\ndefault_range: 1h\n"))
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	return extVal.(*extension)
}

func TestDashboardServes200WithEmbeddedAssets(t *testing.T) {
	e := newTestExtension(t)
	r := newChiRouterForTest(e)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("Content-Type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<script src="app.js">`) {
		t.Error("dashboard HTML missing app.js script tag")
	}
	if !strings.Contains(body, `href="/assets/ext/v1/kit.css"`) {
		t.Error("dashboard HTML missing the design-kit stylesheet link")
	}
	if !strings.Contains(body, "defaultRangeSeconds:  3600") {
		t.Errorf("dashboard HTML did not inject the configured default range (1h = 3600s): %s", body)
	}
	if !strings.Contains(body, "http://tempo:3200") {
		t.Error("dashboard HTML did not inject the configured tempo_url")
	}
}

func TestDashboardOmitsTempoURLWhenUnconfigured(t *testing.T) {
	extVal, err := factory(newTestHost(), []byte("prometheus_url: http://prom:9090\n"))
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	e := extVal.(*extension)
	r := newChiRouterForTest(e)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if strings.Contains(rec.Body.String(), "http://tempo") {
		t.Error("dashboard HTML mentions a tempo URL that was never configured")
	}
}

func TestAppJSServesEmbeddedContent(t *testing.T) {
	e := newTestExtension(t)
	r := newChiRouterForTest(e)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/app.js", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("Content-Type = %q, want a javascript type", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "gen_ai_client_token_usage_total") {
		t.Error("app.js missing the token usage metric constant")
	}
	if !strings.Contains(body, `costNameRegex: 'gen_ai_client_cost(_USD)?_total'`) {
		t.Error("app.js missing the dual-name cost metric regex (the collector appends the USD unit)")
	}
}

// A drive-by edit here would silently change quack.yaml's zero-config behavior.
func TestDefaultRangeWindowConstant(t *testing.T) {
	if defaultRangeWindow != 24*time.Hour {
		t.Errorf("defaultRangeWindow = %v, want 24h", defaultRangeWindow)
	}
}

// The page computes its own step, so the config handoff must not carry one.
func TestDashboardOmitsStepFromConfig(t *testing.T) {
	e := newTestExtension(t)
	r := newChiRouterForTest(e)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if body := rec.Body.String(); strings.Contains(body, "defaultStepSeconds") {
		t.Errorf("dashboard HTML still injects defaultStepSeconds: %s", body)
	}
}

// The page must read quack's SPA theme key (localStorage["theme"]) and stamp data-theme before
// the kit stylesheet loads, or it renders light against a dark host.
func TestDashboardIncludesThemeDetectionScript(t *testing.T) {
	e := newTestExtension(t)
	r := newChiRouterForTest(e)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	if !strings.Contains(body, `localStorage.getItem("theme")`) {
		t.Error(`dashboard HTML missing the theme-detection script reading localStorage["theme"]`)
	}
	if !strings.Contains(body, "document.documentElement.dataset.theme") {
		t.Error("dashboard HTML missing the data-theme stamp")
	}
	scriptIdx := strings.Index(body, `localStorage.getItem("theme")`)
	kitIdx := strings.Index(body, `href="/assets/ext/v1/kit.css"`)
	if scriptIdx < 0 || kitIdx < 0 || scriptIdx > kitIdx {
		t.Error("theme-detection script must run BEFORE the kit stylesheet link, to avoid a flash of the wrong theme")
	}
	if !strings.Contains(body, `:root[data-theme="dark"]`) {
		t.Error("dashboard HTML missing dark-mode fallback CSS scoped to [data-theme=\"dark\"]")
	}
}

// The custom range sits alongside the preset buttons rather than replacing them.
func TestDashboardIncludesCustomRangeControls(t *testing.T) {
	e := newTestExtension(t)
	r := newChiRouterForTest(e)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	for _, want := range []string{
		`id="range-from"`, `id="range-to"`, `id="range-apply"`, `id="range-error"`,
		`type="datetime-local"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard HTML missing custom-range control %q", want)
		}
	}
	// presets must still be present alongside the custom range, not replaced.
	for _, want := range []string{`data-range="1h"`, `data-range="24h"`, `data-range="7d"`, `data-range="30d"`} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard HTML missing preset button %q", want)
		}
	}
}

// TestDashboardIncludesKPIRow pins the sticky totals-row summary tiles.
func TestDashboardIncludesKPIRow(t *testing.T) {
	e := newTestExtension(t)
	r := newChiRouterForTest(e)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	for _, want := range []string{`data-kpi="calls"`, `data-kpi="latency-p95"`, `data-kpi="tokens"`, `data-kpi="cost"`} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard HTML missing KPI tile %q", want)
		}
	}
	if !strings.Contains(body, `class="sticky-bar"`) {
		t.Error("dashboard HTML missing the sticky header/picker/KPI wrapper")
	}
}

// TestDashboardIncludesTimeSeriesPanels pins the token-type, cache savings, cost and latency panels.
func TestDashboardIncludesTimeSeriesPanels(t *testing.T) {
	e := newTestExtension(t)
	r := newChiRouterForTest(e)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	for _, want := range []string{
		`data-panel="tokens-headline"`,
		`data-panel="tokens-model"`,
		`data-panel="tokens-source"`,
		`data-panel="tokens-agent"`,
		`data-panel="tokens-user"`,
		`data-panel="cache-overall"`,
		`data-panel="cache-by-model"`,
		`data-panel="cache-by-agent"`,
		`data-panel="cache-savings"`,
		`data-panel="cost"`,
		`data-panel="cost-cumulative"`,
		`data-panel="latency"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard HTML missing panel mount %q", want)
		}
	}
}

// app.js computes its own query step; pin the nice-step table it rounds up to.
func TestAppJSStepHeuristic(t *testing.T) {
	for _, want := range []string{
		"function stepForSpan(spanSeconds)",
		"const NICE_STEPS = [15, 30, 60, 300, 900, 1800, 3600, 7200, 21600, 43200, 86400, 172800, 604800];",
	} {
		if !strings.Contains(appJS, want) {
			t.Errorf("app.js step heuristic missing %q", want)
		}
	}
}

// The cache section is raw percentages over the window; the ratio-over-time line was noise at
// this call volume and must not come back.
func TestAppJSCacheRateTimeseriesRemoved(t *testing.T) {
	if strings.Contains(appJS, `panelEl("cache-rate")`) {
		t.Error("app.js still mounts the removed cache-rate timeseries panel")
	}
	if strings.Contains(appJS, "ratePoints") {
		t.Error("app.js still builds the removed cache-rate timeseries's per-point series")
	}
	if strings.Contains(indexHTMLSrc, `data-panel="cache-rate"`) {
		t.Error("dashboard HTML still mounts the removed cache-rate timeseries panel")
	}
}

// cacheRateFor must return null, not 0, for zero prompt traffic so an idle
// model/agent is excluded rather than rendered as a fake "0%" row.
func TestAppJSCacheRateFor(t *testing.T) {
	for _, want := range []string{
		"function cacheRateFor(cached, input)",
		"if (volume <= 0) return null;",
		"function cacheRowsFromTotals(totals)",
		"if (rate === null) continue;",
	} {
		if !strings.Contains(appJS, want) {
			t.Errorf("app.js cache-rate computation missing %q", want)
		}
	}
}

// No prompt traffic anywhere in the window is ONE decision for the whole cache section,
// never a fake 0% in some of it.
func TestAppJSCacheSectionSharesOneEmptyDecision(t *testing.T) {
	if !strings.Contains(appJS, "cacheRateFor(cachedTotal, inputTotal) === null") {
		t.Error("app.js cache section must gate its shared empty state on the overall cached+input totals")
	}
	for _, want := range []string{
		"showEmpty(overallEl, EMPTY_TOKENS_MSG);",
		"showEmpty(modelEl, EMPTY_TOKENS_MSG);",
		"showEmpty(agentEl, EMPTY_TOKENS_MSG);",
	} {
		if !strings.Contains(appJS, want) {
			t.Errorf("app.js cache section empty state missing %q", want)
		}
	}
}

// The cache breakdowns sort by volume and come from by-model/by-agent instant queries.
func TestAppJSCacheBreakdownRankedByVolume(t *testing.T) {
	if !strings.Contains(appJS, "rows.sort((a, b) => b.volume - a.volume);") {
		t.Error("app.js cache breakdown rows must sort by volume, busiest first")
	}
	if !strings.Contains(appJS, "function cacheByModelQuery(rangeSeconds)") {
		t.Error("app.js missing the by-model cache instant query builder")
	}
	if !strings.Contains(appJS, "function cacheByAgentQuery(rangeSeconds)") {
		t.Error("app.js missing the by-agent cache instant query builder")
	}
}

// A label whose only token_type is "input" is an embeddings model: cache does not apply, so it
// shows n/a rather than a fake "0.0%".
func TestAppJSCacheBreakdownShowsNAForEmbeddings(t *testing.T) {
	for _, want := range []string{
		`tokenTypeOutput: "output"`,
		"if (cached === 0 && output === 0)",
		"rate: null",
		`row.rate === null ? "n/a" : formatPercent(row.rate)`,
	} {
		if !strings.Contains(appJS, want) {
			t.Errorf("app.js embeddings n/a cache display missing %q", want)
		}
	}
}

// Latency probes for the histogram's _bucket series before rendering percentiles and names
// what's missing otherwise.
func TestAppJSLatencyHasHonestPresenceProbe(t *testing.T) {
	if !strings.Contains(appJS, "_bucket") {
		t.Error("app.js missing a histogram _bucket presence probe for the latency panel")
	}
	if !strings.Contains(appJS, "EMPTY_LATENCY_MSG") {
		t.Error("app.js missing the named latency empty-state message")
	}
}

// Each dimension card has a donut mount beside its series mount, in a two-column split that wraps.
func TestDashboardIncludesDimensionDonuts(t *testing.T) {
	e := newTestExtension(t)
	r := newChiRouterForTest(e)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	for _, want := range []string{
		`data-donut="tokens-model"`,
		`data-donut="tokens-agent"`,
		`data-donut="tokens-source"`,
		`data-donut="tokens-user"`,
		`class="panel-split"`,
		`class="donut-col"`,
		`class="series-col"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard HTML missing donut-card markup %q", want)
		}
	}
}

// The donut and the series beside it share one ranking and one color assigner; two rankings
// would give the same label two colors in one card.
func TestAppJSDonutSharesTheSeriesRanking(t *testing.T) {
	for _, want := range []string{
		"function renderDonut(el, spec)",
		"const DONUT_TOP_N = 6",
		"topKWithOther(entries, DONUT_TOP_N)",
		"const colorFor = makeColorAssigner();",
		`s.isOther ? cssVar("--series-other") : colorFor(s.label)`,
	} {
		if !strings.Contains(appJS, want) {
			t.Errorf("app.js donut missing %q", want)
		}
	}
	// The donut is an instant sum over the window; the series is the range
	// query. Both must go through the two allowlisted proxy endpoints.
	if !strings.Contains(appJS, "promQuery(dimQuery(dimLabel, win.rangeSeconds), win.now)") {
		t.Error("app.js dimension donut must read its window total from an instant proxy query")
	}
	if !strings.Contains(appJS, "promQueryRange(dimQuery(dimLabel, win.step), win.start, win.end, win.step)") {
		t.Error("app.js dimension series must keep using the range proxy query")
	}
}

// Below SPARSE_MAX_NONZERO non-zero points an additive chart draws columns, so a line never
// implies traffic between bursts. Non-additive charts stay lines; 1-2 point series keep dots.
func TestAppJSSwitchesSparseSeriesToBars(t *testing.T) {
	for _, want := range []string{
		"const SPARSE_MAX_NONZERO = 8;",
		"function countNonZeroPoints(points)",
		"function shouldRenderBars(series, additive)",
		"if (!additive) return false;",
		"return most > 0 && most < SPARSE_MAX_NONZERO;",
		"const useBars = shouldRenderBars(visible, additive);",
	} {
		if !strings.Contains(appJS, want) {
			t.Errorf("app.js sparse-series handling missing %q", want)
		}
	}
	if !strings.Contains(appJS, "if (s.points.length <= 2) for (const p of s.points) svg.appendChild(dotMarker(") {
		t.Error("app.js dropped the 1-2 point dot-marker fallback the line path still needs (qx#15)")
	}
}

// Tooltips render Prometheus label values, which this repo does not control, so the page must
// have no HTML sink at all: the only version of the rule a string pin can check.
func TestAppJSBuildsTooltipsWithoutInnerHTML(t *testing.T) {
	for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write"} {
		if strings.Contains(appJS, sink) {
			t.Errorf("app.js uses %s: label values are attacker-influenced data, build nodes with textContent instead", sink)
		}
	}
	if !strings.Contains(appJS, "function tooltipRow(color, text)") {
		t.Error("app.js missing the shared DOM-node tooltip row builder")
	}
}

// Presets and "Custom…" are segments of ONE control and the date row stays collapsed until
// Custom is chosen. Element ids are load-bearing for the page's wiring.
func TestDashboardTimeframeIsOneSegmentedControl(t *testing.T) {
	e := newTestExtension(t)
	r := newChiRouterForTest(e)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	for _, want := range []string{
		`class="segmented"`,
		`class="segment" data-range="1h"`,
		`class="segment segment--custom" id="range-custom"`,
		`aria-controls="custom-range"`,
		`class="custom-range" id="custom-range" hidden`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard HTML missing segmented-picker markup %q", want)
		}
	}
	// Custom must NOT carry data-range: the page's preset handler selects on
	// that attribute, and RANGES has no entry for a custom window.
	if strings.Contains(body, `id="range-custom" data-range`) || strings.Contains(body, `data-range="custom"`) {
		t.Error("the Custom segment must not carry a data-range attribute - the preset handler would treat it as a preset")
	}
	if !strings.Contains(appJS, `document.querySelectorAll("#range-select button[data-range]")`) {
		t.Error("app.js must select presets by data-range so the Custom segment isn't picked up as one")
	}
}

// The cache section has one overall stat, by-model/by-agent tables with header rows, the savings
// tile and the ACP-granularity footnote.
func TestDashboardCacheSectionLayout(t *testing.T) {
	e := newTestExtension(t)
	r := newChiRouterForTest(e)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	body := rec.Body.String()
	for _, want := range []string{
		`id="panel-cache"`,
		`class="cache-overall" data-panel="cache-overall"`,
		`class="cache-breakdown-row"`,
		`data-panel="cache-by-model"`,
		`data-panel="cache-by-agent"`,
		`class="cache-row cache-row-head"`,
		`class="cache-footnote"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("dashboard HTML missing cache-section markup %q", want)
		}
	}
	if !strings.Contains(body, "per-round aggregates") {
		t.Error("dashboard HTML missing the ACP per-round-usage caveat footnote")
	}
}

// gen_ai.token.type lands in Prometheus as gen_ai_token_type; querying the unqualified name
// showed a false "no data" state.
func TestAppJSUsesTranslatedTokenTypeLabel(t *testing.T) {
	if !strings.Contains(appJS, `tokenType: "gen_ai_token_type"`) {
		t.Error("app.js token-type label must be the translated gen_ai_token_type")
	}
	if !strings.Contains(appJS, `tokenTypeFallback: "token_type"`) {
		t.Error("app.js must keep the unqualified token_type fallback for other exporter configs")
	}
}
