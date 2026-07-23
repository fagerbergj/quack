package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fagerbergj/quack/internal/auth"
	"github.com/fagerbergj/quack/internal/config"
)

// TestRequireAuthExceptHealthMethodRestricted tests requireAuthExceptHealth
// in isolation (not through the full chi router, whose own method-not-allowed
// handling for a path with only a GET handler would mask what this
// middleware itself decides): only GET/HEAD to /health skip a's auth check;
// every other method - and every other path - always goes through it.
func TestRequireAuthExceptHealthMethodRestricted(t *testing.T) {
	a, err := auth.New(&config.InboundAuthConfig{
		TrustedHeaders: &config.TrustedHeadersConfig{User: "X-authentik-username"},
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("reached"))
	})
	handler := requireAuthExceptHealth(a)(next)

	tests := []struct {
		name     string
		method   string
		path     string
		wantCode int
	}{
		{name: "GET /health is exempt", method: http.MethodGet, path: "/health", wantCode: http.StatusOK},
		{name: "HEAD /health is exempt", method: http.MethodHead, path: "/health", wantCode: http.StatusOK},
		{name: "POST /health requires auth", method: http.MethodPost, path: "/health", wantCode: http.StatusUnauthorized},
		{name: "PUT /health requires auth", method: http.MethodPut, path: "/health", wantCode: http.StatusUnauthorized},
		{name: "DELETE /health requires auth", method: http.MethodDelete, path: "/health", wantCode: http.StatusUnauthorized},
		{name: "GET elsewhere still requires auth", method: http.MethodGet, path: "/api/v1/chats", wantCode: http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tt.wantCode {
				t.Errorf("%s %s = %d, want %d", tt.method, tt.path, rec.Code, tt.wantCode)
			}
		})
	}
}
