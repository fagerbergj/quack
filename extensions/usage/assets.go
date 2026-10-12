package usage

import (
	_ "embed"
	"html/template"
	"net/http"
)

//go:embed assets/index.html
var indexHTMLSrc string

//go:embed assets/app.js
var appJS string

var indexTmpl = template.Must(template.New("index.html").Parse(indexHTMLSrc))

// dashboardData is the only server->page handoff: everything else (range
// selection, chart data) the page fetches itself from the proxy endpoints.
type dashboardData struct {
	DefaultRangeSeconds int64
	TempoURL            string
}

func (e *extension) handleDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = indexTmpl.Execute(w, dashboardData{
		DefaultRangeSeconds: int64(e.rangeWindow.Seconds()),
		TempoURL:            e.tempoURL,
	})
}

func (e *extension) handleAppJS(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	_, _ = w.Write([]byte(appJS))
}
