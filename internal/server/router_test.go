package server_test

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/go-chi/chi/v5"

	"github.com/fagerbergj/quack/internal/auth"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/server"
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

// TestRouterHealthNonGetNeverReaches200Unauthenticated: through the composed router, a non-GET /health
// without credentials never gets a 200, whichever layer rejects it.
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

// TestRouterServesStaticSPAAssetVerbatim: a real file in the embedded dist (assets/ext/v1/kit.css)
// is served as itself, not swallowed by the index.html fallback.
func TestRouterServesStaticSPAAssetVerbatim(t *testing.T) {
	spa := fstest.MapFS{
		"index.html":                 &fstest.MapFile{Data: []byte("<html>spa</html>")},
		"assets/ext/v1/kit.css":      &fstest.MapFile{Data: []byte(".qk-card{color:red}")},
		"assets/index-4f9a2c1bd8.js": &fstest.MapFile{Data: []byte("console.log('hi')")},
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
	// kit.css is a verbatim public/ file, not a Vite-hashed asset: revalidate every load.
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("kit.css Cache-Control = %q, want no-cache", cc)
	}
	if rec.Header().Get("ETag") == "" {
		t.Errorf("kit.css ETag missing, want a content-hash ETag so revalidation can 304")
	}

	// An unknown client-side route still falls back to index.html.
	req = httptest.NewRequest(http.MethodGet, "/chat/nonexistent", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "<html>spa</html>" {
		t.Errorf("got %d %q, want 200 <html>spa</html> (SPA fallback)", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("index.html Cache-Control = %q, want no-cache (#859: never let the shell go stale)", cc)
	}
	if rec.Header().Get("ETag") == "" {
		t.Errorf("index.html ETag missing, want a content-hash ETag so revalidation can 304")
	}

	// A content-hashed Vite asset gets a year-long immutable cache.
	req = httptest.NewRequest(http.MethodGet, "/assets/index-4f9a2c1bd8.js", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /assets/index-4f9a2c1bd8.js = %d, want 200", rec.Code)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Errorf("hashed asset Cache-Control = %q, want public, max-age=31536000, immutable", cc)
	}
}

// server.ui: none (no SPA) 404s SPA paths without the shell while health and extension routes still answer.
func TestRouterWithoutSPAServesOnlyAPIAndExtensions(t *testing.T) {
	h := server.New(server.Options{
		REST: &rest.Handler{},
		SDKExtensions: []server.SDKExtensionMount{{
			Name: "noop",
			RegisterRoutes: func(_, public chi.Router) {
				public.Get("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
			},
		}},
	})
	for _, tc := range []struct {
		path string
		want int
	}{
		{"/", http.StatusNotFound},
		{"/chat/abc", http.StatusNotFound},
		{"/version.json", http.StatusNotFound},
		{"/assets/index-4f9a2c1bd8.js", http.StatusNotFound},
		{"/health", http.StatusOK},
		{"/noop/", http.StatusOK},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != tc.want {
			t.Errorf("GET %s = %d, want %d", tc.path, rec.Code, tc.want)
		}
		if tc.want == http.StatusNotFound && strings.Contains(rec.Body.String(), "<html") {
			t.Errorf("GET %s served HTML, want no SPA shell", tc.path)
		}
	}
}

// The SPA bundle and JSON API answers are gzipped for a client that accepts it, while an SSE
// stream is left alone so each event still flushes as written.
func TestRouterGzipsAssetsAndJSONButNotSSE(t *testing.T) {
	js := strings.Repeat("console.log('hi');", 200)
	h := server.New(server.Options{
		REST: &rest.Handler{},
		SPA:  fstest.MapFS{"index.html": {Data: []byte("<html/>")}, "assets/index-4f9a2c1bd8.js": {Data: []byte(js)}},
		SDKExtensions: []server.SDKExtensionMount{{Name: "noop", RegisterRoutes: func(_, public chi.Router) {
			public.Get("/events", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte("data: x\n\n"))
			})
		}}},
	})
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Accept-Encoding", "gzip, br")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := get("/assets/index-4f9a2c1bd8.js")
	if rec.Header().Get("Content-Encoding") != "gzip" || rec.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("asset headers = %v, want gzip + Vary: Accept-Encoding", rec.Header())
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	if body, _ := io.ReadAll(zr); string(body) != js {
		t.Errorf("gunzipped asset = %d bytes, want the %d original bytes", len(body), len(js))
	}

	if rec := get("/api/v1/config"); rec.Header().Get("Content-Encoding") != "gzip" {
		t.Errorf("GET /api/v1/config Content-Encoding = %q, want gzip", rec.Header().Get("Content-Encoding"))
	}

	rec = get("/noop/events")
	if enc := rec.Header().Get("Content-Encoding"); enc != "" || rec.Body.String() != "data: x\n\n" {
		t.Errorf("SSE Content-Encoding %q body %q, want an uncompressed stream", enc, rec.Body.String())
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
