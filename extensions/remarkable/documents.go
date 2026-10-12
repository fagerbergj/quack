package remarkable

import (
	"context"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
)

const (
	workflowDocumentIngest = "document-ingest"
	documentsPath          = "/remarkable/documents"

	// maxPerSubmit caps one submit: each selection is a concurrent multi-minute run on one GPU.
	// A cap instead of a queue, deliberately.
	maxPerSubmit = 5
)

type documentsPage struct {
	BaseURL   string
	Docs      []remoteDoc
	ListError string
	Notice    string
}

func (documentsPage) MaxPerSubmit() int { return maxPerSubmit }

// handleDocuments renders 200 with a banner when rmfakecloud is unreachable: this page is the
// nav entry and must not break the nav.
func (e *extension) handleDocuments(w http.ResponseWriter, r *http.Request) {
	page := documentsPage{BaseURL: e.client.baseURL, Notice: submitNotice(r.URL.Query())}

	docs, err := e.client.listDocuments(r.Context())
	if err != nil {
		e.host.Log.Error("remarkable: list documents failed", "err", err)
		page.ListError = err.Error()
	} else {
		sort.Slice(docs, func(i, j int) bool {
			if docs[i].Name == docs[j].Name {
				return docs[i].ID < docs[j].ID
			}
			return docs[i].Name < docs[j].Name
		})
		page.Docs = docs
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := documentsTmpl.Execute(w, page); err != nil {
		e.host.Log.Error("remarkable: documents page render failed", "err", err)
	}
}

// handleIngest dispatches exactly the checked documents. Failures are
// per-document: one bad export doesn't cancel the rest of the batch.
func (e *extension) handleIngest(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	// PostForm, not Form: a ?doc_id= query string must not inject a selection.
	ids := dedupe(r.PostForm["doc_id"])
	if len(ids) == 0 {
		http.Redirect(w, r, documentsPath, http.StatusSeeOther)
		return
	}
	if len(ids) > maxPerSubmit {
		http.Redirect(w, r, documentsPath+"?limit=1", http.StatusSeeOther)
		return
	}

	// The runs already survive request cancellation; the downloads must too,
	// or a client/proxy timeout silently drops the tail of the batch.
	ctx := context.WithoutCancel(r.Context())

	byID := map[string]remoteDoc{}
	docs, err := e.client.listDocuments(ctx)
	if err != nil {
		e.host.Log.Error("remarkable: list documents for ingest failed", "err", err)
	}
	for _, d := range docs {
		byID[d.ID] = d
	}

	queued, failed := 0, 0
	for _, id := range ids {
		d, ok := byID[id]
		if !ok {
			e.host.Log.Error("remarkable: selected document is no longer listed", "doc_id", id)
			failed++
			continue
		}
		if err := e.ingest(ctx, d); err != nil {
			e.host.Log.Error("remarkable: ingest failed", "doc_id", id, "err", err)
			failed++
			continue
		}
		queued++
	}

	if queued > 0 {
		e.mu.Lock()
		err := e.st.save(e.statePath)
		e.mu.Unlock()
		if err != nil {
			e.host.Log.Error("remarkable: save state after ingest failed", "err", err)
		}
	}

	q := url.Values{"queued": {strconv.Itoa(queued)}}
	if failed > 0 {
		q.Set("failed", strconv.Itoa(failed))
	}
	http.Redirect(w, r, documentsPath+"?"+q.Encode(), http.StatusSeeOther)
}

// ingest exports one document and hands it to the document-ingest workflow.
// LocalID is the doc ID, so re-ingesting a note continues its existing chat.
func (e *extension) ingest(ctx context.Context, d remoteDoc) error {
	pdf, err := e.client.downloadPDF(ctx, d.ID)
	if err != nil {
		return err
	}

	req := sdk.DispatchRequest{
		Chat: sdk.ChatRef{
			LocalID: d.ID,
			Title:   d.Name,
			Origin: &sdk.ChatOrigin{
				Extension: extensionName,
				Label:     d.Name,
				Kind:      "document",
				Labels:    buildLabels(d),
			},
		},
		Ask: sdk.Ask{
			Message: fmt.Sprintf("A reMarkable document %q has arrived for ingest.", d.Name),
			Attachments: []sdk.Attachment{{
				// Doc ID, not visibleName: a rename must not change the
				// attachment name that ties document versions together.
				Name: d.ID + ".pdf",
				MIME: "application/pdf",
				Data: pdf,
			}},
		},
		Run: sdk.RunConfig{Workflow: workflowDocumentIngest},
	}
	if err := e.host.Dispatch(ctx, req); err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.st.Documents[d.ID] = docState{
		ID:           d.ID,
		Name:         d.Name,
		Folder:       d.Folder,
		LastModified: d.LastModified,
		InFlight:     true,
		Attempts:     e.st.Documents[d.ID].Attempts + 1,
		UpdatedAt:    time.Now().UTC(),
	}
	return nil
}

// buildLabels carries folder only: rmfakecloud's document list never surfaces tags.
func buildLabels(d remoteDoc) map[string][]sdk.LabelValue {
	if d.Folder == "" {
		return nil
	}
	return map[string][]sdk.LabelValue{
		"folder": {{Value: d.Folder, Display: d.Folder}},
	}
}

// dedupe keeps first-occurrence order; a stale or crafted form posting the
// same ID twice must not start two runs against one chat.
func dedupe(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// submitNotice turns the post-redirect query params into one summary line.
// No flash store exists, and the page has no JS.
func submitNotice(q url.Values) string {
	if q.Has("limit") {
		return fmt.Sprintf("Nothing was queued: select at most %d documents per submit.", maxPerSubmit)
	}
	queued, _ := strconv.Atoi(q.Get("queued"))
	failed, _ := strconv.Atoi(q.Get("failed"))
	switch {
	case queued == 0 && failed == 0:
		return ""
	case failed == 0:
		return fmt.Sprintf("Queued %d document(s).", queued)
	default:
		return fmt.Sprintf("Queued %d document(s); %d failed - see the extension log.", queued, failed)
	}
}

var documentsTmpl = template.Must(template.New("documents").Parse(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<title>reMarkable documents - quack</title>
<link rel="stylesheet" href="` + sdk.UIKitCSS + `">
</head>
<body class="qk-page">
<div class="qk-page__inner">
  <div class="qk-page__header">
    <div>
      <h1>reMarkable documents</h1>
      <p>{{.BaseURL}} &middot; each selection starts a full run immediately &middot; max {{.MaxPerSubmit}} per submit</p>
    </div>
    <a class="qk-btn" href="/remarkable/status">Status</a>
  </div>
  {{with .Notice}}<p><span class="qk-badge">{{.}}</span></p>{{end}}
  {{with .ListError}}<p><span class="qk-badge qk-badge--err">Could not list documents: {{.}}</span></p>{{end}}
  <form method="post" action="/remarkable/documents/ingest">
    <div class="qk-table-wrap">
      <table class="qk-table">
        <thead><tr><th>Ingest</th><th>Document</th><th>Folder</th><th>Modified</th></tr></thead>
        <tbody>
        {{range .Docs}}
          <tr>
            <td><input type="checkbox" name="doc_id" value="{{.ID}}"></td>
            <td>{{.Name}}</td>
            <td>{{.Folder}}</td>
            <td>{{.LastModified.Format "2006-01-02 15:04"}}</td>
          </tr>
        {{else}}
          <tr><td colspan="4">No documents.</td></tr>
        {{end}}
        </tbody>
      </table>
    </div>
    <p><button class="qk-btn" type="submit">Ingest selected</button></p>
  </form>
</div>
</body>
</html>
`))
