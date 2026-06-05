// Package agent turns Quack's declarative agent bundles (an agent-card.json +
// prompt.md plus a config binding for model and built-in tools) into running
// ADK agents, and exposes each one over A2A so the orchestrator dispatches to it
// as an A2A client.
//
// A2A is the orchestrator↔agent protocol from the start. In M1 the agents are
// co-located in the Quack process: each agent's A2A server binds an ephemeral
// loopback port (127.0.0.1:0) and the orchestrator gets the resolved AgentCard
// in-process, so there is no address configuration. Promoting an agent to a
// standalone service later is a config swap (a stable address + the HTTP
// AgentCardProvider), with no change to the agents themselves.
package agent

import (
	"fmt"
	"net"
	"net/http"
	"net/url"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	adkagent "google.golang.org/adk/agent"
	"google.golang.org/adk/agent/remoteagent/v2"
	"google.golang.org/adk/runner"
	adka2a "google.golang.org/adk/server/adka2a/v2"
	"google.golang.org/adk/session"
)

// invokePath is where each agent's A2A JSON-RPC endpoint is mounted.
const invokePath = "/invoke"

// A2AServer is a co-located A2A server exposing one ADK agent over an ephemeral
// loopback port. It owns the listener; Close stops it.
type A2AServer struct {
	// Card is the published AgentCard, with its interface URL pointing at the
	// bound loopback address. Hand it to Client (in-process) — no HTTP
	// resolution needed while co-located.
	Card     *a2a.AgentCard
	listener net.Listener
}

// Serve starts an A2A server for ag on 127.0.0.1:<ephemeral> and returns it with
// the published AgentCard. The agent's session state lives in the shared
// (durable) session service, namespaced under its own app_id (ag.Name()) so it
// stays separate from the orchestrator's "quack" sessions. This is what lets an
// agent's A2A session — keyed by the contextID the orchestrator round-trips —
// survive a process restart, so multi-turn dispatch keeps its context.
func Serve(ag adkagent.Agent, sessions session.Service) (*A2AServer, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("agent %q: a2a listen: %w", ag.Name(), err)
	}
	baseURL := &url.URL{Scheme: "http", Host: listener.Addr().String()}

	card := &a2a.AgentCard{
		Name:        ag.Name(),
		Description: ag.Description(),
		SupportedInterfaces: []*a2a.AgentInterface{{
			URL:             baseURL.JoinPath(invokePath).String(),
			ProtocolBinding: a2a.TransportProtocolJSONRPC,
			ProtocolVersion: a2a.Version,
		}},
		Version:            "1.0.0",
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills:             adka2a.BuildAgentSkills(ag),
		Capabilities:       a2a.AgentCapabilities{Streaming: true},
	}

	executor := adka2a.NewExecutor(adka2a.ExecutorConfig{
		RunnerConfig: runner.Config{
			AppName:           ag.Name(),
			Agent:             ag,
			SessionService:    sessions,
			AutoCreateSession: true,
		},
		// Stream each ADK event as its own artifact so the agent's thinking /
		// tool_call / tool_result activity surfaces live to the orchestrator,
		// rather than being aggregated into one final artifact.
		OutputMode: adka2a.OutputArtifactPerEvent,
	})

	mux := http.NewServeMux()
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	mux.Handle(invokePath, a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(executor)))

	go func() { _ = http.Serve(listener, mux) }()

	return &A2AServer{Card: card, listener: listener}, nil
}

// Close stops the A2A server's listener.
func (s *A2AServer) Close() error { return s.listener.Close() }

// Client returns an ADK agent that dispatches to this server over A2A. Use it as
// a sub-agent of the orchestrator; its Name matches the served agent's, so
// transfer-to-agent targets it correctly.
func (s *A2AServer) Client() (adkagent.Agent, error) {
	return remoteagent.NewA2A(remoteagent.A2AConfig{
		Name:        s.Card.Name,
		Description: s.Card.Description,
		AgentCard:   s.Card,
	})
}
