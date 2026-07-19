package rest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fagerbergj/quack/internal/schema"
)

func TestGetCapabilities(t *testing.T) {
	tests := []struct {
		name          string
		githubEnabled bool
		want          bool
	}{
		{name: "github configured", githubEnabled: true, want: true},
		{name: "github unconfigured", githubEnabled: false, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestHandler(t)
			h.githubEnabled = tt.githubEnabled

			req := httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil)
			rr := httptest.NewRecorder()
			h.GetCapabilities(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rr.Code)
			}
			var got schema.Capabilities
			if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.Github.Enabled != tt.want {
				t.Errorf("Github.Enabled = %v, want %v", got.Github.Enabled, tt.want)
			}
		})
	}
}
