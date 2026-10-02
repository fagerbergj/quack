package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/langfuse"
	"github.com/fagerbergj/quack/internal/schema"
)

func sp(s string) *string   { return &s }
func fp(f float64) *float64 { return &f }

func rec(chat, node, top, base string, topP float64, confident bool) schema.DecisionRecord {
	r := schema.DecisionRecord{ChatId: chat, NodeId: sp(node), Round: sp("r0"), Point: "plan.accept", Handler: "clef", Mode: "observe",
		Outcome: "observe", Confident: confident, Top: sp(top), TopP: fp(topP), LatencyMs: 100, InputTokens: new(int),
		State: map[string]any{"chat": chat}, Questions: []any{"q"}}
	*r.InputTokens = 1000
	if base != "" {
		r.Baseline = sp(base)
	}
	return r
}

func fixture() []schema.DecisionRecord {
	unavail := rec("c4", "n", "", "accept", 0, false)
	unavail.Outcome, unavail.Error = "unavailable", sp("timeout")
	score := rec("c5", "n", "2", "3", 0.9, true)
	score.Point = "review.level"
	return []schema.DecisionRecord{
		rec("c1", "n", "accept", "accept", 0.95, true),
		rec("c2", "n", "accept", "reject", 0.92, true), // confident disagreement
		rec("c3", "n", "reject", "accept", 0.5, false), // unconfident disagreement
		unavail,
		rec("c6", "n", "accept", "", 0.99, true), // no baseline: counted, not compared
		score,
	}
}

func TestBuildDecisionReport(t *testing.T) {
	rep := BuildDecisionReport(fixture())
	if len(rep.Points) != 2 || rep.Points[0].Point != "plan.accept" {
		t.Fatalf("points = %+v", rep.Points)
	}
	p := rep.Points[0]
	if p.N != 5 || p.Unavailable != 1 || p.Answered != 4 || p.Confident != 3 || p.Compared != 3 || p.Agree != 1 {
		t.Errorf("counts = %+v", p)
	}
	if got := *p.Coverage; got != 0.75 {
		t.Errorf("coverage = %v, want 0.75", got)
	}
	if got := *p.Agreement; got < 0.333 || got > 0.334 {
		t.Errorf("agreement = %v, want 1/3", got)
	}
	if p.ConfidentDisagreements != 1 || p.DisagreementList[0] != (Disagreement{Chat: "c2", Node: "n", Top: "accept", Baseline: "reject", TopP: 0.92}) {
		t.Errorf("disagreements = %+v", p.DisagreementList)
	}
	if p.Confusion["accept"]["reject"] != 1 || p.Confusion["reject"]["accept"] != 1 || p.Confusion["accept"]["accept"] != 1 {
		t.Errorf("confusion = %+v", p.Confusion)
	}
	if p.MeanLatencyMS != 100 || p.P50LatencyMS != 100 || p.P50InputTokens != 1000 {
		t.Errorf("latency/tokens = %+v", p)
	}
	if s := rep.Points[1]; s.Confusion["2"]["3"] != 1 || *s.Agreement != 0 {
		t.Errorf("score point = %+v", s)
	}
	if rep.Totals.N != 6 || rep.Totals.Unavailable != 1 || rep.Totals.Compared != 4 || rep.Totals.Agree != 1 {
		t.Errorf("totals = %+v", rep.Totals)
	}
}

func TestBuildDecisionReportNoAnswers(t *testing.T) {
	r := rec("c", "n", "", "accept", 0, false)
	r.Outcome = "unavailable"
	p := BuildDecisionReport([]schema.DecisionRecord{r}).Points[0]
	if p.Coverage != nil || p.Agreement != nil {
		t.Errorf("ratios over no answers must be nil, got %v %v", p.Coverage, p.Agreement)
	}
}

func TestMedian(t *testing.T) {
	if got := median([]float64{4, 1, 3, 2}); got != 2.5 {
		t.Errorf("median = %v", got)
	}
}

func TestWriteReportTable(t *testing.T) {
	var b bytes.Buffer
	if err := writeReportTable(&b, BuildDecisionReport(fixture())); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"plan.accept", "75%", "33%", "total", "confident disagreement: chat c2"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("table missing %q:\n%s", want, b.String())
		}
	}
}

func TestDecisionItemID(t *testing.T) {
	a := rec("c1", "n", "accept", "accept", 0.9, true)
	b := a
	b.Top, b.Handler = sp("reject"), "other" // answer and handler are not part of the key
	if DecisionItemID(a) != DecisionItemID(b) {
		t.Error("id must not depend on the model's answer")
	}
	for name, mut := range map[string]func(*schema.DecisionRecord){
		"chat":  func(r *schema.DecisionRecord) { r.ChatId = "c2" },
		"node":  func(r *schema.DecisionRecord) { r.NodeId = sp("m") },
		"round": func(r *schema.DecisionRecord) { r.Round = sp("r1") },
		"point": func(r *schema.DecisionRecord) { r.Point = "x" },
		"state": func(r *schema.DecisionRecord) { r.State = map[string]any{"chat": "other"} },
	} {
		c := a
		mut(&c)
		if DecisionItemID(a) == DecisionItemID(c) {
			t.Errorf("id ignores %s", name)
		}
	}
}

type lfCall struct {
	method, path, auth string
	body               map[string]any
}

func fakeDecisionLangfuse(t *testing.T, calls *[]lfCall) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		user, _, _ := r.BasicAuth()
		*calls = append(*calls, lfCall{r.Method, r.URL.Path, user, body})
		switch {
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/api/public/ingestion":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"successes":[],"errors":[]}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"x","name":"n","projectId":"p","datasetId":"d","datasetName":"n","datasetItemId":"i","traceId":"t","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`))
		}
	}))
}

func TestExportDecisions(t *testing.T) {
	var calls []lfCall
	srv := fakeDecisionLangfuse(t, &calls)
	defer srv.Close()
	lf := newTestGenClient(t, srv)
	ing := langfuse.New(srv.URL, "pk", "sk", langfuse.WithHTTPClient(srv.Client()))
	recs := fixture()
	recs[0].At = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	sums, err := ExportDecisions(context.Background(), lf, ing, "0.62.0", recs)
	if err != nil {
		t.Fatal(err)
	}
	if len(sums) != 2 || sums[0].Items+sums[1].Items != 5 { // c6 has no baseline
		t.Fatalf("summaries = %+v", sums)
	}
	var datasets, items, runItems []lfCall
	scores := map[string]float64{}
	outputs := map[string]map[string]any{}
	for _, c := range calls {
		if c.auth != "pk" {
			t.Errorf("%s %s auth user = %q", c.method, c.path, c.auth)
		}
		switch c.path {
		case "/api/public/v2/datasets":
			if c.method == http.MethodPost {
				datasets = append(datasets, c)
			}
		case "/api/public/dataset-items":
			items = append(items, c)
		case "/api/public/dataset-run-items":
			runItems = append(runItems, c)
		case "/api/public/ingestion":
			for _, e := range c.body["batch"].([]any) {
				ev := e.(map[string]any)
				if ev["type"] == "trace-create" {
					b := ev["body"].(map[string]any)
					outputs[b["id"].(string)] = b["output"].(map[string]any)
				}
				if ev["type"] == "score-create" {
					b := ev["body"].(map[string]any)
					scores[b["traceId"].(string)+"/"+b["name"].(string)] = b["value"].(float64)
				}
			}
		}
	}
	if len(datasets) != 2 {
		t.Errorf("datasets = %+v", datasets)
	}
	if len(items) != 5 || len(runItems) != 5 {
		t.Fatalf("items=%d runItems=%d, want 5/5", len(items), len(runItems))
	}
	wantID := DecisionItemID(recs[0])
	var first map[string]any
	for _, it := range items {
		if it.body["id"] == wantID {
			first = it.body
		}
	}
	var ri map[string]any
	for _, it := range runItems {
		if it.body["datasetItemId"] == wantID {
			ri = it.body
		}
	}
	exp := first["expectedOutput"].(map[string]any)
	meta := first["metadata"].(map[string]any)
	in := first["input"].(map[string]any)
	if first["datasetName"] != "decisions/plan.accept" || exp["baseline"] != "accept" || meta["chat"] != "c1" || meta["quack_version"] != "0.62.0" ||
		meta["handler"] != "clef" || meta["mode"] != "observe" || meta["node"] != "n" || meta["round"] != "r0" || in["state"] == nil || in["questions"] == nil {
		t.Errorf("item body = %+v", first)
	}
	if ri["runName"] != "clef@0.62.0" {
		t.Errorf("run item = %+v", ri)
	}
	trace := ri["traceId"].(string)
	if scores[trace+"/agrees_with_baseline"] != 1 || scores[trace+"/top_p"] != 0.95 {
		t.Errorf("scores for %s = %+v", trace, scores)
	}
	if o := outputs[trace]; o["top"] != "accept" || o["top_p"] != 0.95 || o["outcome"] != "observe" || o["confident"] != true {
		t.Errorf("trace output = %+v", o)
	}
	if len(scores) != 8 { // 4 answered decisions x 2; the unavailable one is unscored
		t.Errorf("scores = %d, want 8: %+v", len(scores), scores)
	}
}

func TestExportDecisionsIdempotentIDs(t *testing.T) {
	var c1, c2 []lfCall
	run := func(calls *[]lfCall) {
		srv := fakeDecisionLangfuse(t, calls)
		defer srv.Close()
		ing := langfuse.New(srv.URL, "pk", "sk", langfuse.WithHTTPClient(srv.Client()))
		if _, err := ExportDecisions(context.Background(), newTestGenClient(t, srv), ing, "v", fixture()); err != nil {
			t.Fatal(err)
		}
	}
	run(&c1)
	run(&c2)
	if len(c1) != len(c2) {
		t.Fatalf("call counts differ: %d vs %d", len(c1), len(c2))
	}
	for i := range c1 {
		a, _ := json.Marshal(c1[i].body)
		b, _ := json.Marshal(c2[i].body)
		if c1[i].path != c2[i].path || string(a) != string(b) {
			t.Fatalf("call %d differs between exports:\n%s\n%s", i, a, b)
		}
	}
}

func TestIngestReportsEventErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = w.Write([]byte(`{"successes":[],"errors":[{"id":"e1","status":400,"message":"bad"}]}`))
	}))
	defer srv.Close()
	ing := langfuse.New(srv.URL, "pk", "sk", langfuse.WithHTTPClient(srv.Client()))
	if err := ing.Ingest(context.Background(), []langfuse.IngestEvent{{ID: "e1"}}); err == nil || !strings.Contains(err.Error(), "bad") {
		t.Errorf("err = %v", err)
	}
}

func decisionsServer(t *testing.T, status int, seen *string) *httptest.Server {
	t.Setenv("QUACK_HOME", t.TempDir())
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = r.URL.RawQuery
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(schema.DecisionList{QuackVersion: "1.2.3", Data: fixture()})
	}))
}

func TestRunDecisionsReport(t *testing.T) {
	var q string
	srv := decisionsServer(t, http.StatusOK, &q)
	defer srv.Close()
	f := DecisionFilter{Since: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), Point: "plan.accept", Chats: []string{"c1", "c2"}}
	var out bytes.Buffer
	if err := RunDecisionsReport(context.Background(), &out, srv.URL, f, true); err != nil {
		t.Fatal(err)
	}
	var rep DecisionReport
	if err := json.Unmarshal(out.Bytes(), &rep); err != nil || len(rep.Points) != 2 {
		t.Fatalf("json = %v %s", err, out.String())
	}
	for _, want := range []string{"since=2026-10-01T00%3A00%3A00Z", "point=plan.accept", "chat=c1&chat=c2"} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q missing %q", q, want)
		}
	}
	out.Reset()
	if err := RunDecisionsReport(context.Background(), &out, srv.URL, DecisionFilter{}, false); err != nil || !strings.Contains(out.String(), "POINT") {
		t.Errorf("table = %v %s", err, out.String())
	}
}

func TestRunDecisionsReportEmptyAndNoLedger(t *testing.T) {
	var out bytes.Buffer
	if err := writeReportTable(&out, DecisionReport{}); err != nil || !strings.Contains(out.String(), "No decisions") {
		t.Errorf("empty = %v %q", err, out.String())
	}
	var q string
	srv := decisionsServer(t, http.StatusNotFound, &q)
	defer srv.Close()
	err := RunDecisionsReport(context.Background(), &out, srv.URL, DecisionFilter{}, false)
	if err == nil || !strings.Contains(err.Error(), "no ledger store") {
		t.Errorf("err = %v", err)
	}
}

func TestRunDecisionsExport(t *testing.T) {
	var q string
	srv := decisionsServer(t, http.StatusOK, &q)
	defer srv.Close()
	var calls []lfCall
	lfSrv := fakeDecisionLangfuse(t, &calls)
	defer lfSrv.Close()
	ing := langfuse.New(lfSrv.URL, "pk", "sk", langfuse.WithHTTPClient(lfSrv.Client()))
	var out bytes.Buffer
	if err := RunDecisionsExport(context.Background(), &out, srv.URL, DecisionFilter{}, newTestGenClient(t, lfSrv), ing); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(q, "with_state=true") || !strings.Contains(out.String(), "decisions/plan.accept: 4 item(s), runs clef@1.2.3") {
		t.Errorf("query %q, out %q", q, out.String())
	}
}
