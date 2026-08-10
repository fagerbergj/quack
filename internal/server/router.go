// Package server wires Quack's HTTP surface: REST routes, MCP server, and embedded SPA.
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
	"github.com/fagerbergj/quack/internal/server/adkdebug"
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
	// ADKDebug is adkdebug.Mount.Handler, gated by config observability.adk_debug
	// (default off). It runs agents ungated (see adkdebug package doc) - mounted
	// INSIDE the auth group deliberately, never as an unauthenticated extension.
	ADKDebug http.Handler
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

	// Extension webhooks outside the auth group - they authenticate via their own signatures.
	for _, ext := range opts.Extensions {
		ext.RegisterRoutes(r)
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
