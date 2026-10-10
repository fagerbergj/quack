package langfuse

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListDatasetItemsDecodesPagesOverMaxOKBody(t *testing.T) {
	big := strings.Repeat("x", 2*maxOKBody)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/public/dataset-items" || r.URL.Query().Get("datasetName") != "ds" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": "i1", "input": map[string]string{"task": big}}},
			"meta": map[string]int{"totalPages": 1},
		})
	}))
	defer srv.Close()

	items, pages, err := New(srv.URL, "pk", "sk", WithHTTPClient(srv.Client())).ListDatasetItems(context.Background(), "ds", 1)
	if err != nil || pages != 1 || len(items) != 1 || items[0].ID != "i1" {
		t.Fatalf("ListDatasetItems = %v, %d, %v", items, pages, err)
	}
}
