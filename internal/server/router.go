// Package server wires Quack's HTTP surface: the generated REST routes, the
// optional MCP server, and the embedded single-page app.
package server

import (
	"io/fs"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/server/rest"
)

// MCPPath is where the Streamable-HTTP MCP server is mounted.
const MCPPath = "/api/v1/research/mcp"

// Options configure the router.
type Options struct {
	REST *rest.Handler
	MCP  http.Handler // optional Streamable-HTTP MCP handler
	SPA  fs.FS        // optional embedded frontend dist
}

// New builds the HTTP handler.
func New(opts Options) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	if opts.MCP != nil {
		r.Handle(MCPPath, opts.MCP)
		r.Handle(MCPPath+"/*", opts.MCP)
	}

	// Generated REST routing (registers /health + chat endpoints on r).
	schema.HandlerFromMux(opts.REST, r)

	// Serve the SPA for everything else (client-side routing falls back to index.html).
	if opts.SPA != nil {
		r.NotFound(spaHandler(opts.SPA))
	}
	return r
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
