package remarkable

import (
	"encoding/json"
	"html/template"
	"net/http"
	"sort"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
)

// statusResponse is the dispatch-tracking snapshot shared by both /status
// (HTML) and /status.json (the same data, unrendered, for tooling).
type statusResponse struct {
	Extension string      `json:"extension"`
	BaseURL   string      `json:"base_url"`
	Documents []statusDoc `json:"documents"`
}

type statusDoc struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Folder       string    `json:"folder,omitempty"`
	LastModified time.Time `json:"last_modified"`
	InFlight     bool      `json:"in_flight"`
	LastOutcome  string    `json:"last_outcome,omitempty"`
	LastError    string    `json:"last_error,omitempty"`
	Attempts     int       `json:"attempts"`
}

// Badge is the .qk-badge modifier for this row - "" is the neutral
// (never-run) case, deliberately not "ok".
func (d statusDoc) Badge() string {
	switch {
	case d.InFlight:
		return "warn"
	case d.LastOutcome == "failed":
		return "err"
	case d.LastOutcome == "done":
		return "ok"
	case d.LastOutcome == "needs_input":
		return "warn"
	default:
		return ""
	}
}

// Label is the badge's visible text.
func (d statusDoc) Label() string {
	switch {
	case d.InFlight:
		return "in flight"
	case d.LastOutcome == "":
		return "never run"
	default:
		return d.LastOutcome
	}
}

func (e *extension) statusSnapshot() statusResponse {
	e.mu.Lock()
	defer e.mu.Unlock()

	docs := map[string]docState{}
	if e.st != nil {
		docs = e.st.Documents
	}
	resp := statusResponse{
		Extension: extensionName,
		BaseURL:   e.client.baseURL,
		Documents: make([]statusDoc, 0, len(docs)),
	}
	for _, d := range docs {
		resp.Documents = append(resp.Documents, statusDoc{
			ID:           d.ID,
			Name:         d.Name,
			Folder:       d.Folder,
			LastModified: d.LastModified,
			InFlight:     d.InFlight,
			LastOutcome:  d.LastOutcome,
			LastError:    d.LastError,
			Attempts:     d.Attempts,
		})
	}
	sort.Slice(resp.Documents, func(i, j int) bool { return resp.Documents[i].ID < resp.Documents[j].ID })
	return resp
}

// handleStatus renders the human-facing page (sdk.UI's Href target);
// handleStatusJSON serves machine consumers.
func (e *extension) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := statusTmpl.Execute(w, e.statusSnapshot()); err != nil {
		e.host.Log.Error("remarkable: status page render failed", "err", err)
	}
}

func (e *extension) handleStatusJSON(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(e.statusSnapshot()); err != nil {
		e.host.Log.Error("remarkable: encode status JSON failed", "err", err)
	}
}

var statusTmpl = template.Must(template.New("status").Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<title>reMarkable - quack</title>
<link rel="stylesheet" href="` + sdk.UIKitCSS + `">
</head>
<body class="qk-page">
<div class="qk-page__inner">
  <div class="qk-page__header">
    <div>
      <h1>reMarkable</h1>
      <p>{{.BaseURL}}</p>
    </div>
    <div>
      <a class="qk-btn" href="/remarkable/documents">Documents</a>
      <a class="qk-btn" href="/remarkable/status.json">JSON</a>
    </div>
  </div>
  <div class="qk-table-wrap">
    <table class="qk-table">
      <thead><tr><th>Document</th><th>Folder</th><th>Modified</th><th>Attempts</th><th>Status</th></tr></thead>
      <tbody>
      {{range .Documents}}
        <tr>
          <td><a href="/chat/ext:remarkable:{{.ID}}" target="_top">{{.Name}}</a></td>
          <td>{{.Folder}}</td>
          <td>{{.LastModified.Format "2006-01-02 15:04"}}</td>
          <td>{{.Attempts}}</td>
          <td>
            <span class="qk-badge{{with .Badge}} qk-badge--{{.}}{{end}}">{{.Label}}</span>
            {{with .LastError}}<span style="color:var(--qk-muted)">{{.}}</span>{{end}}
          </td>
        </tr>
      {{else}}
        <tr><td colspan="5">No documents dispatched yet.</td></tr>
      {{end}}
      </tbody>
    </table>
  </div>
</div>
</body>
</html>
`))
