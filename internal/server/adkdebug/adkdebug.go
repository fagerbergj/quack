// Package adkdebug mounts ADK's REST debug surface and Angular console onto quack's session service
// and agents, for local inspection only.
package adkdebug

import (
	"fmt"
	"net/http"
	"sort"

	"github.com/gorilla/mux"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/artifact"
	adklauncher "google.golang.org/adk/v2/cmd/launcher"
	"google.golang.org/adk/v2/cmd/launcher/web/webui"
	"google.golang.org/adk/v2/server/adkrest"
	"google.golang.org/adk/v2/session"
)

// MountPath is where Handler must be mounted: webui bakes it into the API base URL it hands the browser,
// so mounting elsewhere breaks the console.
const MountPath = "/debug/adk"

const apiPrefix = "/api"

// Mount is ADK's REST controllers plus console, wired to quack's session.Service and agents. SECURITY:
// /run, /run_sse and /run_live execute agents with no trust gate or auth, and can't be dropped separately.
type Mount struct {
	// Handler serves the combined surface, rooted as if mounted at "/" -
	// callers strip MountPath before delegating (see router.go).
	Handler http.Handler

	srv *adkrest.Server
}

// SpanProcessor feeds ADK's in-memory /debug/trace store; register it on the
// live TracerProvider. (No log twin: sdklog processors are constructor-only.)
func (m *Mount) SpanProcessor() sdktrace.SpanProcessor { return m.srv.SpanProcessor() }

// New builds Mount. agents seeds the AgentLoader; which one becomes "root"
// doesn't matter, no adkrest controller distinguishes it from the rest.
func New(sessions session.Service, agents map[string]adkagent.Agent, artifacts artifact.Service) (*Mount, error) {
	if len(agents) == 0 {
		return nil, fmt.Errorf("adkdebug: at least one agent required")
	}
	names := make([]string, 0, len(agents))
	for n := range agents {
		names = append(names, n)
	}
	sort.Strings(names)
	var others []adkagent.Agent
	for _, n := range names[1:] {
		others = append(others, agents[n])
	}
	loader, err := adkagent.NewMultiLoader(agents[names[0]], others...)
	if err != nil {
		return nil, fmt.Errorf("adkdebug: agent loader: %w", err)
	}

	if artifacts == nil {
		artifacts = artifact.InMemoryService()
	}

	srv, err := adkrest.NewServer(adkrest.ServerConfig{
		SessionService:  sessions,
		AgentLoader:     loader,
		ArtifactService: artifacts,
		// adk v2.3.0 made the debug router (incl. /debug/trace, what this
		// package exists to mount) opt-in; this whole Mount is pointless without it.
		DebugAPIConfig: adkrest.DebugAPIConfig{IncludeDebugAPI: true},
	})
	if err != nil {
		return nil, fmt.Errorf("adkdebug: adkrest server: %w", err)
	}

	router := mux.NewRouter().StrictSlash(true)
	router.PathPrefix(apiPrefix).Handler(http.StripPrefix(apiPrefix, srv))

	wl := webui.NewLauncher()
	// backendAddress is browser-relative and must include MountPath: the browser can't see the
	// server-side StripPrefix.
	if _, err := wl.Parse([]string{"-api_server_address", MountPath + apiPrefix}); err != nil {
		return nil, fmt.Errorf("adkdebug: webui flags: %w", err)
	}
	// SetupSubrouters ignores its *launcher.Config (see cmd/launcher/web/webui/webui.go),
	// so the zero value is fine.
	if err := wl.SetupSubrouters(router, &adklauncher.Config{}); err != nil {
		return nil, fmt.Errorf("adkdebug: webui mount: %w", err)
	}

	return &Mount{Handler: router, srv: srv}, nil
}
