// Package server wires Quack's HTTP surface: the generated REST routes, the
// optional MCP server, and the embedded single-page app.
package server

import (
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/fagerbergj/quack/internal/auth"
	"github.com/fagerbergj/quack/internal/extension"
	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/server/rest"
)

// MCPPath is where the Streamable-HTTP MCP server is mounted.
const MCPPath = "/api/v1/mcp"

// Options configure the router.
type Options struct {
	REST       *rest.Handler
	MCP        http.Handler          // optional Streamable-HTTP MCP handler
	SPA        fs.FS                 // optional embedded frontend dist
	Extensions []extension.Extension // optional inbound routes (e.g. GitHub webhook)
	Auth       *auth.Auth            // optional inbound auth (nil = disabled, open)
}

// New builds the HTTP handler.
func New(opts Options) http.Handler {
	r := chi.NewRouter()
	r.Use(requestLogger) // slog (not chi's stderr logger) so QUACK_LOG_LEVEL gates it
	r.Use(middleware.Recoverer)

	// MCP + the generated REST routes are the auth-gated API surface. Scoped to
	// this Group so it never wraps extension webhooks (verified by their own
	// signature scheme, e.g. GitHub's HMAC) or the SPA/NotFound fallback below.
	r.Group(func(r chi.Router) {
		r.Use(requireAuthExceptHealth(opts.Auth))

		if opts.MCP != nil {
			r.Handle(MCPPath, opts.MCP)
			r.Handle(MCPPath+"/*", opts.MCP)
		}

		// Generated REST routing (registers /health + chat endpoints on r).
		schema.HandlerFromMux(opts.REST, r)
	})

	// Extension inbound routes (webhooks/callbacks), mounted as raw handlers -
	// same as MCP, NOT via the OpenAPI codegen (their payloads are the external
	// system's schema, not quack's client API contract). Deliberately outside
	// the auth Group above: a webhook authenticates via its own signature.
	for _, ext := range opts.Extensions {
		ext.RegisterRoutes(r)
	}

	// Unmatched routes: an unknown /api/ path is a real 404 (JSON) - don't fall
	// through to the SPA, or API clients (and `quack api`) get index.html with 200.
	// Everything else serves the SPA so client-side routing works.
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

// requestLogger logs one line per request through slog at Info, so the process
// log level controls it - unlike chi's middleware.Logger, which writes to stderr
// unconditionally and would pollute `quack -p` (the in-process duck runs at warn).
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

// requireAuthExceptHealth wraps a's middleware but always lets /health through
// unauthenticated - it's a liveness probe (docker-compose healthcheck), not
// part of the API surface auth protects.
func requireAuthExceptHealth(a *auth.Auth) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		protected := a.Middleware(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/health" {
				next.ServeHTTP(w, r)
				return
			}
			protected.ServeHTTP(w, r)
		})
	}
}

func spaHandler(spa fs.FS) http.HandlerFunc {
	fileServer := http.FileServer(http.FS(spa))
	return func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(spa, p); err != nil {
			http.ServeFileFS(w, r, spa, "index.html")
			return
		}
		fileServer.ServeHTTP(w, r)
	}
}
