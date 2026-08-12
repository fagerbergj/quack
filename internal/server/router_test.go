package server_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/go-chi/chi/v5"

	"github.com/fagerbergj/quack/internal/auth"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/server"
	"github.com/fagerbergj/quack/internal/server/adkdebug"
	"github.com/fagerbergj/quack/internal/server/rest"
)

// stubHandler echoes 200 + a fixed body - a stand-in for the MCP mount, which
// is just an http.Handler as far as the router is concerned.
func stubHandler(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	})
}

func TestRouterHealthAlwaysPublic(t *testing.T) {
	a, err := auth.New(&config.InboundAuthConfig{
		TrustedHeaders: &config.TrustedHeadersConfig{User: "X-authentik-username"},
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	h := server.New(server.Options{
		REST: &rest.Handler{}, // HealthCheck touches no field
		MCP:  stubHandler("mcp"),
		Auth: a,
	})

	// No trusted header, no bearer token - /health must still succeed.
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET /health with auth configured = %d, want 200", rec.Code)
	}
}

// TestRouterHealthNonGetNeverReaches200Unauthenticated is the composed-stack
// companion to TestRequireAuthExceptHealthMethodRestricted (package server):
// with only GET registered for /health, a non-GET request never reaches an
// authenticated 200 without credentials - chi's own method-not-allowed
// handling covers it, but this pins the observable contract regardless of
// which layer produces the rejection.
func TestRouterHealthNonGetNeverReaches200Unauthenticated(t *testing.T) {
	a, err := auth.New(&config.InboundAuthConfig{
		TrustedHeaders: &config.TrustedHeadersConfig{User: "X-authentik-username"},
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	h := server.New(server.Options{
		REST: &rest.Handler{},
		MCP:  stubHandler("mcp"),
		Auth: a,
	})

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/health", nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code == http.StatusOK {
				t.Errorf("%s /health with no identity = 200, want a rejection", method)
			}
		})
	}
}

func TestRouterMCPGatedWhenAuthConfigured(t *testing.T) {
	a, err := auth.New(&config.InboundAuthConfig{
		TrustedHeaders: &config.TrustedHeadersConfig{User: "X-authentik-username"},
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	h := server.New(server.Options{
		REST: &rest.Handler{},
		MCP:  stubHandler("mcp"),
		Auth: a,
	})

	tests := []struct {
		name     string
		header   string
		wantCode int
	}{
		{name: "no identity -> unauthorized", wantCode: http.StatusUnauthorized},
		{name: "trusted header -> reaches mcp", header: "jason", wantCode: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, server.MCPPath, nil)
			if tt.header != "" {
				req.Header.Set("X-authentik-username", tt.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
		})
	}
}

func TestRouterMCPOpenWhenAuthUnconfigured(t *testing.T) {
	h := server.New(server.Options{
		REST: &rest.Handler{},
		MCP:  stubHandler("mcp"),
		Auth: nil, // auth: absent - today's open behavior, unchanged
	})

	req := httptest.NewRequest(http.MethodGet, server.MCPPath, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "mcp" {
		t.Errorf("got %d %q, want 200 mcp", rec.Code, rec.Body.String())
	}
}

func TestRouterADKDebugGatedWhenAuthConfigured(t *testing.T) {
	a, err := auth.New(&config.InboundAuthConfig{
		TrustedHeaders: &config.TrustedHeadersConfig{User: "X-authentik-username"},
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	h := server.New(server.Options{
		REST:     &rest.Handler{},
		Auth:     a,
		ADKDebug: stubHandler("adkdebug"),
	})

	tests := []struct {
		name     string
		header   string
		wantCode int
	}{
		{name: "no identity -> unauthorized", wantCode: http.StatusUnauthorized},
		{name: "trusted header -> reaches adkdebug", header: "jason", wantCode: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, adkdebug.MountPath+"/api/list-apps", nil)
			if tt.header != "" {
				req.Header.Set("X-authentik-username", tt.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantCode)
			}
		})
	}
}

func TestRouterADKDebugAbsentByDefault(t *testing.T) {
	h := server.New(server.Options{REST: &rest.Handler{}})

	req := httptest.NewRequest(http.MethodGet, adkdebug.MountPath+"/api/list-apps", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Errorf("ADKDebug unset (default) should not be reachable, got 200")
	}
}

// TestRouterMountsSDKExtensionAtBareName pins the route shape Jason asked
// for: an SDK extension mounts at its bare name, not a shared /ext/ prefix.
func TestRouterMountsSDKExtensionAtBareName(t *testing.T) {
	h := server.New(server.Options{
		REST: &rest.Handler{},
		SDKExtensions: []server.SDKExtensionMount{{
			Name: "noop",
			RegisterRoutes: func(authed, public chi.Router) {
				public.Get("/status", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
			},
		}},
	})

	req := httptest.NewRequest(http.MethodGet, "/noop/status", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET /noop/status = %d, want 200 (bare-name mount)", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/ext/noop/status", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Errorf("GET /ext/noop/status = %d, want it NOT mounted under the old /ext/ prefix", rec.Code)
	}
}

// TestRouterServesStaticSPAAssetVerbatim pins the split spaHandler relies
// on: a real file under the embedded dist (e.g. frontend/public's
// assets/ext/v1/kit.css, copied through verbatim by Vite) is served as
// itself, not swallowed by the index.html client-route fallback.
func TestRouterServesStaticSPAAssetVerbatim(t *testing.T) {
	spa := fstest.MapFS{
		"index.html":            &fstest.MapFile{Data: []byte("<html>spa</html>")},
		"assets/ext/v1/kit.css": &fstest.MapFile{Data: []byte(".qk-card{color:red}")},
	}
	h := server.New(server.Options{REST: &rest.Handler{}, SPA: spa})

	req := httptest.NewRequest(http.MethodGet, "/assets/ext/v1/kit.css", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /assets/ext/v1/kit.css = %d, want 200", rec.Code)
	}
	if got := rec.Body.String(); got != ".qk-card{color:red}" {
		t.Errorf("body = %q, want the kit.css bytes verbatim (not index.html)", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/css; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/css", ct)
	}

	// An unknown client-side route still falls back to index.html.
	req = httptest.NewRequest(http.MethodGet, "/chat/nonexistent", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "<html>spa</html>" {
		t.Errorf("got %d %q, want 200 <html>spa</html> (SPA fallback)", rec.Code, rec.Body.String())
	}
}

func TestRouterUnknownAPIPathIsJSON404RegardlessOfAuth(t *testing.T) {
	a, err := auth.New(&config.InboundAuthConfig{
		TrustedHeaders: &config.TrustedHeadersConfig{User: "X-authentik-username"},
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	h := server.New(server.Options{REST: &rest.Handler{}, Auth: a})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/nonexistent", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}
