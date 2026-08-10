// Package adkdebug mounts ADK's own REST debug surface (server/adkrest) and
// its Angular debug console (cmd/launcher/web/webui) onto quack's session
// service and agent registry, for local inspection - not a product surface.
package adkdebug

import (
	"fmt"
	"net/http"
	"sort"

	"github.com/gorilla/mux"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/artifact"
	adklauncher "google.golang.org/adk/v2/cmd/launcher"
	"google.golang.org/adk/v2/cmd/launcher/web/webui"
	"google.golang.org/adk/v2/server/adkrest"
	"google.golang.org/adk/v2/session"
)

// MountPath is where callers must mount Handler - webui's runtime-config.json
// bakes this path into the API base URL it hands the browser, so mounting
// Handler anywhere else breaks the console (the REST routes still work fine).
const MountPath = "/debug/adk"

const apiPrefix = "/api"

// Mount is ADK's own session/runtime/debug/artifact REST controllers plus
// its Angular console, wired to quack's real session.Service and agents.
//
// SECURITY: /run, /run_sse and /run_live execute a loaded agent directly -
// no trust gate, no auth of their own, and no way to drop just those routes
// (adkrest.NewServer wires them with the read-only ones as one unit).
type Mount struct {
	// Handler serves the combined surface, rooted as if mounted at "/" -
	// callers strip MountPath before delegating (see router.go).
	Handler http.Handler

	srv *adkrest.Server
}

// SpanProcessor/LogProcessor feed ADK's in-memory /debug/trace store; register
// them on the live TracerProvider/LoggerProvider for it to populate -
// unregistered, the mount still serves but /debug/trace stays empty.
func (m *Mount) SpanProcessor() sdktrace.SpanProcessor { return m.srv.SpanProcessor() }
func (m *Mount) LogProcessor() sdklog.Processor        { return m.srv.LogProcessor() }

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
	})
	if err != nil {
		return nil, fmt.Errorf("adkdebug: adkrest server: %w", err)
	}

	router := mux.NewRouter().StrictSlash(true)
	router.PathPrefix(apiPrefix).Handler(http.StripPrefix(apiPrefix, srv))

	wl := webui.NewLauncher()
	// backendAddress is browser-relative and must include MountPath - the
	// browser fetches it directly, unaware this handler gets StripPrefix'd
	// server-side before reaching the mux router built above.
	if _, err := wl.Parse([]string{"-api_server_address", MountPath + apiPrefix}); err != nil {
		return nil, fmt.Errorf("adkdebug: webui flags: %w", err)
	}
	// webUILauncher.SetupSubrouters ignores the *launcher.Config argument
	// entirely (confirmed in cmd/launcher/web/webui/webui.go) - the zero
	// value is fine.
	if err := wl.SetupSubrouters(router, &adklauncher.Config{}); err != nil {
		return nil, fmt.Errorf("adkdebug: webui mount: %w", err)
	}

	return &Mount{Handler: router, srv: srv}, nil
}
