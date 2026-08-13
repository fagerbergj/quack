package rest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fagerbergj/quack/internal/schema"
)

func strp(s string) *string { return &s }

func TestListExtensions_Empty(t *testing.T) {
	h := newTestHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/extensions", nil)
	rec := httptest.NewRecorder()
	h.ListExtensions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out []schema.ExtensionInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("out = %+v, want empty", out)
	}
}

// TestListExtensions_NameOnlyAndWithUI covers both shapes the SDK's
// optional UI descriptor produces: a module implementing it gets
// title/href/icon, one that doesn't stays name-only.
func TestListExtensions_NameOnlyAndWithUI(t *testing.T) {
	h := newTestHandler(t)
	h.extensions = []schema.ExtensionInfo{
		{Name: "noop"},
		{Name: "remarkable", Title: strp("reMarkable"), Href: strp("/remarkable/review"), Icon: strp("📄")},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/extensions", nil)
	rec := httptest.NewRecorder()
	h.ListExtensions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out []schema.ExtensionInfo
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("out = %+v, want 2 entries", out)
	}
	if out[0].Name != "noop" || out[0].Title != nil || out[0].Href != nil || out[0].Icon != nil {
		t.Errorf("out[0] = %+v, want name-only noop", out[0])
	}
	if out[1].Name != "remarkable" || out[1].Title == nil || *out[1].Title != "reMarkable" || out[1].Href == nil || *out[1].Href != "/remarkable/review" || out[1].Icon == nil || *out[1].Icon != "📄" {
		t.Errorf("out[1] = %+v, want remarkable with title/href/icon", out[1])
	}
}
