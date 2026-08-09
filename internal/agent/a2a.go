// Package agent builds ADK agents from declarative bundles.
package agent

import (
	"context"
	"fmt"
	"iter"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/remoteagent/v2"
	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/runner"
	adka2a "google.golang.org/adk/v2/server/adka2a/v2"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"

	"github.com/fagerbergj/quack/internal/httpx"
)

// invokePath is where each agent's A2A JSON-RPC endpoint is mounted.
const invokePath = "/invoke"

// A2AServer: co-located A2A server for one ADK agent.
type A2AServer struct {
	// Published AgentCard with loopback URL.
	Card     *a2a.AgentCard
	listener net.Listener
}

// Serve starts an A2A server for ag on 127.0.0.1:<ephemeral> and returns it with the AgentCard.
func Serve(ag adkagent.Agent, sessions session.Service, mem adkmemory.Service) (*A2AServer, error) {
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
		Skills:             buildSkills(ag),
		Capabilities:       a2a.AgentCapabilities{Streaming: true},
	}

	executor := adka2a.NewExecutor(adka2a.ExecutorConfig{
		RunnerConfig: runner.Config{
			AppName:           ag.Name(),
			Agent:             ag,
			SessionService:    sessions,
			MemoryService:     mem,
			AutoCreateSession: true,
		},
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

// buildSkills returns the A2A skills for ag.
func buildSkills(ag adkagent.Agent) []a2a.AgentSkill {
	return adka2a.BuildAgentSkills(ag)
}

// Client returns an ADK agent that dispatches to this server over A2A.
func (s *A2AServer) Client() (adkagent.Agent, error) {
	base, err := s.clientNamed(s.Card.Name)
	if err != nil {
		return nil, err
	}
	return nodeClient{Agent: base, srv: s}, nil
}

// nodeClient is an A2A client agent with per-node identity (ForNode).
type nodeClient struct {
	adkagent.Agent
	srv *A2AServer
}

// ForNode returns a client identity unique to nodeKey, working around an ADK
// remote-session collision bug for concurrent sibling nodes.
func (c nodeClient) ForNode(nodeKey string) (adkagent.Agent, error) {
	return c.srv.clientNamed(c.srv.Card.Name + "#" + nodeKey)
}

// ClientForNode is Client() with the per-node identity fix pre-applied.
func (s *A2AServer) ClientForNode(nodeKey string) (adkagent.Agent, error) {
	return s.clientNamed(s.Card.Name + "#" + nodeKey)
}

// clientNamed builds a remote agent for this server under the given local name.
func (s *A2AServer) clientNamed(name string) (adkagent.Agent, error) {
	factory := a2aclient.NewFactory(
		a2aclient.WithJSONRPCTransport(&http.Client{Transport: httpx.NewTransport(nil)}),
	)
	base := remoteagent.NewA2AClientProvider(factory)
	return remoteagent.NewA2A(remoteagent.A2AConfig{
		Name:        name,
		Description: s.Card.Description,
		AgentCard:   s.Card,
		ClientProvider: func(ctx context.Context, card *a2a.AgentCard) (remoteagent.A2AClient, error) {
			c, err := base(ctx, card)
			if err != nil {
				return nil, err
			}
			return scopedClient{A2AClient: c}, nil
		},
		GenAIPartConverter:        sanitizeWorkflowPlumbingPart,
		RemoteTaskCleanupCallback: func(context.Context, *a2a.AgentCard, remoteagent.A2AClient, a2a.TaskInfo, error) {},
	})
}

// scopedClient filters sibling node events from outbound A2A messages by
// invocation + branch, since the part converter sees already-synthetic events.
type scopedClient struct{ remoteagent.A2AClient }

func (c scopedClient) SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	scopeMessage(ctx, req)
	return c.A2AClient.SendMessage(ctx, req)
}

func (c scopedClient) SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	scopeMessage(ctx, req)
	return c.A2AClient.SendStreamingMessage(ctx, req)
}

// scopeMessage rewrites req's parts in place, scoped to invocation + branch.
func scopeMessage(ctx context.Context, req *a2a.SendMessageRequest) {
	ic, ok := ctx.(adkagent.InvocationContext)
	if !ok || req == nil || req.Message == nil || ic.Session() == nil {
		return
	}
	events := ic.Session().Events()
	start := 0
	for i := events.Len() - 1; i >= 0; i-- {
		if ev := events.At(i); ev != nil && ev.Author == ic.Agent().Name() {
			start = i + 1
			break
		}
	}
	parts := make([]*a2a.Part, 0, len(req.Message.Parts))
	for i := start; i < events.Len(); i++ {
		ev := events.At(i)
		if ev == nil || ev.Content == nil || ev.InvocationID != ic.InvocationID() || !eventBelongsToBranch(ic.Branch(), ev) {
			continue
		}
		if ev.Author != "user" && ev.Author != ic.Agent().Name() {
			parts = append(parts, describeEvent(ev)...)
			continue
		}
		for _, p := range ev.Content.Parts {
			cp, err := sanitizeWorkflowPlumbingPart(ic, ev, p)
			if err != nil {
				slog.Warn("a2a: part conversion failed; dropping", "component", "agent", "err", err)
				continue
			}
			if cp != nil {
				parts = append(parts, cp)
			}
		}
	}
	req.Message.Parts = parts
}

// describeEvent renders a foreign-authored event as user-facing text.
func describeEvent(ev *session.Event) []*a2a.Part {
	parts := make([]*a2a.Part, 0, len(ev.Content.Parts)+1)
	for _, p := range ev.Content.Parts {
		switch {
		case p == nil || p.Thought:
		case p.Text != "":
			parts = append(parts, a2a.NewTextPart(fmt.Sprintf("[%s] said: %s", ev.Author, p.Text)))
		case p.FunctionCall != nil:
			parts = append(parts, a2a.NewTextPart(fmt.Sprintf("[%s] called tool %s with parameters: %v", ev.Author, p.FunctionCall.Name, p.FunctionCall.Args)))
		case p.FunctionResponse != nil:
			parts = append(parts, a2a.NewTextPart(fmt.Sprintf("[%s] %s tool returned result: %v", ev.Author, p.FunctionResponse.Name, p.FunctionResponse.Response)))
		case p.InlineData != nil || p.FileData != nil:
			mp, err := adka2a.ToA2APart(p, ev.LongRunningToolIDs)
			if err != nil {
				slog.Warn("a2a: media part conversion failed; dropping", "component", "agent", "author", ev.Author, "err", err)
				continue
			}
			parts = append(parts, mp)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return append([]*a2a.Part{a2a.NewTextPart("For context:")}, parts...)
}

// sanitizeWorkflowPlumbingPart neutralizes HITL/resume FunctionCall/Response
// pairs before they cross the A2A wire (mismatched pairing otherwise causes
// silent empty completions on the remote server).
func sanitizeWorkflowPlumbingPart(ctx context.Context, adkEvent *session.Event, part *genai.Part) (*a2a.Part, error) {
	if part == nil {
		return nil, nil
	}
	if ic, ok := ctx.(interface{ Branch() string }); ok && !eventBelongsToBranch(ic.Branch(), adkEvent) {
		return nil, nil
	}
	switch {
	case part.FunctionCall != nil && part.FunctionCall.Name == workflow.WorkflowInputFunctionCallName:
		return a2a.NewTextPart(fmt.Sprintf("[%s] asked: %v", adkEvent.Author, part.FunctionCall.Args)), nil
	case part.FunctionResponse != nil && part.FunctionResponse.Name == workflow.WorkflowInputFunctionCallName:
		return a2a.NewTextPart(fmt.Sprintf("[%s] answered: %v", adkEvent.Author, part.FunctionResponse.Response)), nil
	default:
		return adka2a.ToA2APart(part, adkEvent.LongRunningToolIDs)
	}
}

// eventBelongsToBranch reproduces ADK's own branch-visibility rule for the A2A
// path where ADK omits it.
func eventBelongsToBranch(invocationBranch string, ev *session.Event) bool {
	if invocationBranch == "" || ev.Branch == "" || ev.Branch == invocationBranch {
		return true
	}
	return strings.HasPrefix(invocationBranch, ev.Branch+".")
}
