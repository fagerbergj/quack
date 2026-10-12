package usage

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// chartHarness evaluates app.js under node against a minimal fake DOM and prints what the
// renderer drew, so gap handling is checked by behavior rather than by source text.
const chartHarness = `
function el(tag) {
  return {
    tag, attrs: {}, children: [], style: {}, firstChild: null, text: "",
    setAttribute(k, v) { this.attrs[k] = String(v); },
    appendChild(c) { this.children.push(c); this.firstChild = this.children[0]; return c; },
    append(...cs) { cs.forEach((c) => this.appendChild(c)); },
    insertBefore(c, ref) {
      const i = this.children.indexOf(ref);
      this.children.splice(i < 0 ? this.children.length : i, 0, c);
      this.firstChild = this.children[0];
      return c;
    },
    removeChild(c) { this.children.splice(this.children.indexOf(c), 1); this.firstChild = this.children[0] || null; return c; },
    addEventListener() {},
    set textContent(v) { this.text = v; },
    get textContent() { return this.text; },
  };
}
const document = { addEventListener() {}, createElement: el, createElementNS: (_, t) => el(t) };
const src = require("fs").readFileSync(0, "utf8");
const app = new Function("document", src + ";return {renderSeriesChart, latencyPresenceQuery};")(document);
function walk(n, f) { f(n); (n.children || []).forEach((c) => walk(c, f)); }
function draw(mode, points, end) {
  const root = el("div");
  app.renderSeriesChart(root, { series: [{ key: "a", label: "a", color: "c", points }], start: 0, end, step: 60, mode });
  const out = { polygon: 0, polyline: 0, circle: 0, notes: [] };
  walk(root, (n) => {
    if (n.tag in out) out[n.tag]++;
    if (n.text && n.text.startsWith("no data since")) out.notes.push(n.text);
  });
  return out;
}
const gapped = [0, 60, 120, 180, 240, 300, 360, 420, 480, 540, 1200].map((t) => ({ t, v: 1 }));
const full = [0, 60, 120, 180].map((t) => ({ t, v: 1 }));
console.log(JSON.stringify({
  stacked: draw("stacked-area", gapped, 2400),
  lines: draw("lines", gapped, 2400),
  current: draw("lines", full, 180),
  presence: app.latencyPresenceQuery(3600),
}));
`

type chartDraw struct {
	Polygon  int      `json:"polygon"`
	Polyline int      `json:"polyline"`
	Circle   int      `json:"circle"`
	Notes    []string `json:"notes"`
}

func TestChartDrawsGapsAsGapsAndFlagsStaleData(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node unavailable in this environment")
	}
	cmd := exec.Command("node", "-e", chartHarness)
	cmd.Stdin = strings.NewReader(appJS)
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node harness: %v\n%s", err, raw)
	}
	var got struct {
		Stacked, Lines, Current chartDraw
		Presence                string
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode harness output %q: %v", raw, err)
	}
	// One band and top line per contiguous run, so the 540..1200 gap is never bridged; the lone
	// sample after it is marked with a dot.
	if got.Stacked.Polygon != 2 || got.Stacked.Polyline != 2 || got.Stacked.Circle != 1 {
		t.Errorf("stacked-area across a gap drew %+v, want 2 bands, 2 lines, 1 dot", got.Stacked)
	}
	if got.Lines.Polyline != 2 || got.Lines.Circle != 1 {
		t.Errorf("line across a gap drew %+v, want 2 segments and 1 dot", got.Lines)
	}
	if len(got.Stacked.Notes) != 1 || len(got.Lines.Notes) != 1 {
		t.Errorf("series ending at 1200s of a 2400s range: notes %v / %v, want one \"no data since\" each", got.Stacked.Notes, got.Lines.Notes)
	}
	if got.Current.Polyline != 1 || len(got.Current.Notes) != 0 {
		t.Errorf("contiguous series reaching end drew %d lines and notes %v, want 1 and none", got.Current.Polyline, got.Current.Notes)
	}
	if got.Presence != "count(last_over_time(quack_model_call_duration_seconds_bucket[3600s]))" {
		t.Errorf("latencyPresenceQuery(3600) = %q, want a range lookback", got.Presence)
	}
}
