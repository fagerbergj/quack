package rest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fagerbergj/quack/internal/schema"
)

// TestGetConfigVersion: the version stamped into the handler (build flag ->
// main.version -> serve.Version -> NewHandler) reaches the wire response.
func TestGetConfigVersion(t *testing.T) {
	h := newTestHandler(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
	h.GetConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out schema.ClientConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Version == nil || *out.Version != "test" {
		t.Errorf("version = %v, want \"test\"", out.Version)
	}
}
