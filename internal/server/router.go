// Package server wires Quack's HTTP surface: REST routes, MCP server, and embedded SPA.
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/fagerbergj/quack/internal/auth"
	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/server/adkdebug"
	"github.com/fagerbergj/quack/internal/server/rest"
)

// MCPPath is where the Streamable-HTTP MCP server is mounted.
const MCPPath = "/api/v1/mcp"

// Options configure the router.
type Options struct {
	REST          *rest.Handler
	MCP           http.Handler        // optional Streamable-HTTP MCP handler
	SPA           fs.FS               // optional embedded frontend dist
	SDKExtensions []SDKExtensionMount // optional quack-extensions SDK modules, mounted at /<name>
	Auth          *auth.Auth          // optional inbound auth (nil = disabled, open)
	// ADKDebug is adkdebug.Mount.Handler, gated by config observability.adk_debug
	// (default off). It runs agents ungated (see adkdebug package doc) - mounted
	// INSIDE the auth group deliberately, never as an unauthenticated extension.
	ADKDebug http.Handler
}

// SDKExtensionMount is one quack-extensions SDK module's route registration,
// mounted at /<name>/: an authed router (session auth) and a public one
// (webhook-class) - mirrors sdk.Extension.RegisterRoutes so serve can pass it
// straight through. Name must pass ValidateExtensionName before it reaches
// here - the caller (internal/serve) is expected to check at startup, not
// this constructor.
type SDKExtensionMount struct {
	Name           string
	RegisterRoutes func(authed, public chi.Router)
}

// New builds the HTTP handler.
func New(opts Options) http.Handler {
	r := chi.NewRouter()
	r.Use(requestLogger) // slog (not chi's stderr logger) so QUACK_LOG_LEVEL gates it
	r.Use(middleware.Recoverer)

	// MCP + REST are auth-gated; extensions authenticate via their own schemes.
	r.Group(func(r chi.Router) {
		r.Use(requireAuthExceptHealth(opts.Auth))

		if opts.MCP != nil {
			r.Handle(MCPPath, opts.MCP)
			r.Handle(MCPPath+"/*", opts.MCP)
		}

		if opts.ADKDebug != nil {
			r.Mount(adkdebug.MountPath, http.StripPrefix(adkdebug.MountPath, opts.ADKDebug))
		}

		schema.HandlerFromMux(opts.REST, r)
	})

	// ONE mount per extension: authed routes go through a middleware-wrapped
	// view of the same router, avoiding a duplicate-mount conflict with public.
	// Mounted at the bare name, not a shared /ext/ prefix - ValidateExtensionName
	// is what keeps a name from shadowing the SPA/API surface below.
	for _, m := range opts.SDKExtensions {
		combined := chi.NewRouter()
		authed := combined.With(requireAuthExceptHealth(opts.Auth))
		m.RegisterRoutes(authed, combined)
		r.Mount("/"+m.Name, combined)
	}

	// Unmatched /api/ paths 404 (JSON); everything else serves the SPA so client-side routing works.
	r.NotFound(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasPrefix(req.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"not found"}`))
			return
		}
		if opts.SPA != nil {
			spaHandler(opts.SPA)(w, req)
			return
		}
		http.NotFound(w, req)
	})
	return r
}

// slog-based request logger (controlled by QUACK_LOG_LEVEL, unlike chi's stderr logger).
func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		start := time.Now()
		defer func() {
			slog.Info("http request", "component", "server",
				"method", r.Method, "path", r.URL.Path,
				"status", ww.Status(), "bytes", ww.BytesWritten(), "dur", time.Since(start))
		}()
		next.ServeHTTP(ww, r)
	})
}

// Always lets GET/HEAD /health through (liveness probe), blocks other methods on that path.
func requireAuthExceptHealth(a *auth.Auth) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		protected := a.Middleware(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
				next.ServeHTTP(w, r)
				return
			}
			protected.ServeHTTP(w, r)
		})
	}
}

func spaHandler(spa fs.FS) http.HandlerFunc {
	fileServer := http.FileServer(http.FS(spa))
	etags := buildETags(spa)
	return func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(spa, p); err != nil {
			setCacheHeaders(w, "index.html", etags)
			http.ServeFileFS(w, r, spa, "index.html")
			return
		}
		setCacheHeaders(w, p, etags)
		fileServer.ServeHTTP(w, r)
	}
}

// hashedAssetRE matches Vite's fingerprinted output (assets/name-<hash>.ext);
// deliberately conservative so a verbatim-copied public/ file (e.g.
// assets/ext/v1/kit.css) never gets mistaken for one.
var hashedAssetRE = regexp.MustCompile(`-[0-9A-Za-z_]{8,}\.[0-9A-Za-z]+$`)

func isHashedAsset(p string) bool {
	return strings.HasPrefix(p, "assets/") && hashedAssetRE.MatchString(p)
}

// setCacheHeaders implements #859: a hashed Vite asset is immutable for a
// year, everything else (index.html, verbatim public/ files) revalidates on
// every load via ETag so a stale SPA shell can't survive a deploy.
func setCacheHeaders(w http.ResponseWriter, p string, etags map[string]string) {
	if isHashedAsset(p) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	if etag, ok := etags[p]; ok {
		w.Header().Set("ETag", etag) // net/http honors a pre-set ETag for If-None-Match -> 304
	}
}

// buildETags hashes every non-hashed-asset file once at startup - the
// embedded FS is fixed for the process lifetime (and its modtimes are zero,
// so http.FileServer's own Last-Modified path never fires).
func buildETags(spa fs.FS) map[string]string {
	etags := make(map[string]string)
	_ = fs.WalkDir(spa, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || isHashedAsset(p) {
			return nil
		}
		data, rerr := fs.ReadFile(spa, p)
		if rerr != nil {
			return nil
		}
		sum := sha256.Sum256(data)
		etags[p] = `"` + hex.EncodeToString(sum[:16]) + `"`
		return nil
	})
	return etags
}
