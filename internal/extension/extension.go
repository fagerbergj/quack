// Package extension is quack's seam for a bundled unit that contributes BOTH
// directions of an integration behind ONE auth context it owns:
//
//   - OUTBOUND - Tools the agent calls, authed as the extension (e.g. a GitHub
//     App installation token): its capabilities to act on the external system.
//   - INBOUND - a webhook/callback route (RegisterRoutes) that dispatches runs
//     on triggering events.
//
// This is deliberately thin: one interface, wired at startup. There is no
// plugin loader, marketplace, or dynamic discovery - the GitHub extension
// (internal/github) is the only implementation today. Add a second by writing
// another Extension and building it in internal/serve, nothing more.
package extension

import (
	"google.golang.org/adk/v2/tool"

	"github.com/go-chi/chi/v5"
)

// Extension is a bundled inbound+outbound integration. An enabled extension is
// built once at startup: its Tools() join every agent's tool set (alongside the
// builtins and the skill toolset), and RegisterRoutes mounts its webhook routes
// on the main router (mirroring how the MCP handler is mounted in
// internal/server/router.go).
type Extension interface {
	// Name identifies the extension in logs (e.g. "github").
	Name() string
	// Tools are the outbound capabilities, authed via the extension's own auth
	// context. Empty is valid (an inbound-only extension).
	Tools() []tool.Tool
	// RegisterRoutes mounts the extension's inbound routes (webhooks/callbacks)
	// on r. Called once at router-build time.
	RegisterRoutes(r chi.Router)
}
