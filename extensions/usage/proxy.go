package usage

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// promAPITimeout bounds each proxied Prometheus call so a hung upstream
// can't leak a goroutine per dashboard request.
const promAPITimeout = 10 * time.Second

// The only params ever forwarded upstream; anything else is dropped, so no upstream path or
// param name is ever user-controlled.
var (
	queryRangeParams = []string{"query", "start", "end", "step"}
	queryParams      = []string{"query", "time"}
)

// prometheusProxy forwards GET-only requests to exactly two fixed upstream
// paths under baseURL. It never proxies an arbitrary path.
type prometheusProxy struct {
	baseURL string
	client  *http.Client
	log     *slog.Logger
}

func newPrometheusProxy(baseURL string, log *slog.Logger) *prometheusProxy {
	if log == nil {
		log = slog.Default()
	}
	return &prometheusProxy{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: promAPITimeout},
		log:     log,
	}
}

func (p *prometheusProxy) handleQueryRange(w http.ResponseWriter, r *http.Request) {
	p.forward(w, r, "/api/v1/query_range", queryRangeParams)
}

func (p *prometheusProxy) handleQuery(w http.ResponseWriter, r *http.Request) {
	p.forward(w, r, "/api/v1/query", queryParams)
}

// forward is the only place that talks to Prometheus. path is always one of
// the two constants above, never derived from the request.
func (p *prometheusProxy) forward(w http.ResponseWriter, r *http.Request, path string, allowed []string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	q := url.Values{}
	incoming := r.URL.Query()
	for _, name := range allowed {
		if v := incoming.Get(name); v != "" {
			q.Set(name, v)
		}
	}
	if q.Get("query") == "" {
		writePromError(w, http.StatusBadRequest, "query param is required")
		return
	}

	target := p.baseURL + path + "?" + q.Encode()
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil {
		writePromError(w, http.StatusInternalServerError, "build upstream request: "+err.Error())
		return
	}

	resp, err := p.client.Do(req)
	if err != nil {
		// The raw error (dial errors, DNS) can name internal hosts/ports -
		// log it, but never put it in the response to an authenticated user.
		p.log.Error("usage: prometheus upstream unreachable", "err", err)
		writePromError(w, http.StatusBadGateway, "prometheus unreachable")
		return
	}
	defer func() { _ = resp.Body.Close() }()

	// Prometheus JSON, verbatim - status code and body untouched.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// writePromError mirrors Prometheus's own error shape so the page handles one response
// shape whether the failure was ours or upstream's.
func writePromError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"status":    "error",
		"errorType": "unknown",
		"error":     msg,
	})
}
