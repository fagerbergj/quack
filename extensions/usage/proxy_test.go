package usage

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const fixtureVectorJSON = `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"model":"gpt-4"},"value":[1700000000,"123"]}]}}`
const fixtureMatrixJSON = `{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1700000000,"1.5"],[1700000060,"2.5"]]}]}}`

// fakePrometheus records every request it receives and serves a fixed body,
// so tests can assert exactly what the proxy forwarded upstream.
type fakePrometheus struct {
	*httptest.Server
	lastPath  string
	lastQuery url.Values
	body      string
	status    int
}

func newFakePrometheus(t *testing.T, body string) *fakePrometheus {
	t.Helper()
	fp := &fakePrometheus{body: body, status: http.StatusOK}
	fp.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fp.lastPath = r.URL.Path
		fp.lastQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fp.status)
		_, _ = w.Write([]byte(fp.body))
	}))
	t.Cleanup(fp.Close)
	return fp
}

func newTestProxyRouter(baseURL string) http.Handler {
	p := newPrometheusProxy(baseURL, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/query_range", p.handleQueryRange)
	mux.HandleFunc("/api/query", p.handleQuery)
	return mux
}

func TestProxyQueryRangeForwardsAllowedParamsOnly(t *testing.T) {
	fp := newFakePrometheus(t, fixtureMatrixJSON)
	r := newTestProxyRouter(fp.URL)

	req := httptest.NewRequest(http.MethodGet, "/api/query_range?query=up&start=100&end=200&step=15&foo=drop-me&match[]=drop-me-too", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if fp.lastPath != "/api/v1/query_range" {
		t.Fatalf("upstream path = %q, want /api/v1/query_range", fp.lastPath)
	}
	want := url.Values{"query": {"up"}, "start": {"100"}, "end": {"200"}, "step": {"15"}}
	if fp.lastQuery.Encode() != want.Encode() {
		t.Fatalf("upstream query = %v, want %v", fp.lastQuery, want)
	}
	if rec.Body.String() != fixtureMatrixJSON {
		t.Fatalf("body = %s, want verbatim upstream body", rec.Body.String())
	}
}

// A custom range (arbitrary epoch seconds and step) still forwards only
// query/start/end/step verbatim; extras like "timeout" are dropped.
func TestProxyQueryRangeForwardsCustomRangeParamShapes(t *testing.T) {
	fp := newFakePrometheus(t, fixtureMatrixJSON)
	r := newTestProxyRouter(fp.URL)

	start := int64(1732000000)
	end := int64(1732000000 + 11*24*60*60 + 3*60*60 + 17*60) // an arbitrary ~11.1 day custom span
	step := int64(7200)                                      // app.js stepForSpan for this span
	query := "sum by (model) (increase(gen_ai_client_token_usage_total[" + fmtInt(step) + "s]))"

	target := fmt.Sprintf("/api/query_range?query=%s&start=%d&end=%d&step=%d&timeout=30s",
		url.QueryEscape(query), start, end, step)
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	want := url.Values{
		"query": {query},
		"start": {fmtInt(start)},
		"end":   {fmtInt(end)},
		"step":  {fmtInt(step)},
	}
	if fp.lastQuery.Encode() != want.Encode() {
		t.Fatalf("upstream query = %v, want %v (custom-range shape, timeout dropped)", fp.lastQuery, want)
	}
}

func fmtInt(n int64) string { return fmt.Sprintf("%d", n) }

func TestProxyQueryForwardsAllowedParamsOnly(t *testing.T) {
	fp := newFakePrometheus(t, fixtureVectorJSON)
	r := newTestProxyRouter(fp.URL)

	// start/end/step are not in the /api/query allowlist - only query/time are.
	req := httptest.NewRequest(http.MethodGet, "/api/query?query=up&time=100&start=999&end=999&step=999", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	if fp.lastPath != "/api/v1/query" {
		t.Fatalf("upstream path = %q, want /api/v1/query", fp.lastPath)
	}
	want := url.Values{"query": {"up"}, "time": {"100"}}
	if fp.lastQuery.Encode() != want.Encode() {
		t.Fatalf("upstream query = %v, want %v (start/end/step must be dropped)", fp.lastQuery, want)
	}
}

func TestProxyReturnsUpstreamBodyAndStatusVerbatim(t *testing.T) {
	fp := newFakePrometheus(t, fixtureVectorJSON)
	r := newTestProxyRouter(fp.URL)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/query?query=up&time=100", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	if rec.Body.String() != fixtureVectorJSON {
		t.Errorf("body = %s, want %s", rec.Body.String(), fixtureVectorJSON)
	}
}

func TestProxyRejectsNonGET(t *testing.T) {
	fp := newFakePrometheus(t, fixtureVectorJSON)
	r := newTestProxyRouter(fp.URL)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(method, "/api/query?query=up&time=100", nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want 405", method, rec.Code)
		}
	}
	if fp.lastPath != "" {
		t.Errorf("upstream was contacted on a rejected method: %q", fp.lastPath)
	}
}

func TestProxyRequiresQueryParam(t *testing.T) {
	fp := newFakePrometheus(t, fixtureVectorJSON)
	r := newTestProxyRouter(fp.URL)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/query?time=100", nil))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["status"] != "error" {
		t.Errorf("status field = %q, want %q", got["status"], "error")
	}
	if fp.lastPath != "" {
		t.Errorf("upstream was contacted despite the missing query param: %q", fp.lastPath)
	}
}

func TestProxyReportsUpstreamUnreachable(t *testing.T) {
	fp := newFakePrometheus(t, fixtureVectorJSON)
	fp.Close() // baseURL now points at a closed listener

	r := newTestProxyRouter(fp.URL)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/query?query=up&time=100", nil))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502, body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["status"] != "error" {
		t.Errorf("status field = %q, want %q", got["status"], "error")
	}
	if strings.Contains(got["error"], fp.URL) || strings.Contains(got["error"], "127.0.0.1") {
		t.Errorf("error message leaked the upstream address to the client: %q", got["error"])
	}
}

// The routes cover exactly query_range and query; nothing else, including a path smuggled
// via a query param, reaches Prometheus.
func TestProxyNeverExposesArbitraryUpstreamPaths(t *testing.T) {
	fp := newFakePrometheus(t, fixtureVectorJSON)
	e := &extension{proxy: newPrometheusProxy(fp.URL, nil)}

	authed := newChiRouterForTest(e)

	rec := httptest.NewRecorder()
	authed.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/reload", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unregistered upstream-shaped path status = %d, want 404", rec.Code)
	}
	if fp.lastPath != "" {
		t.Fatalf("upstream was contacted for an unregistered path: %q", fp.lastPath)
	}
}
