// Package agent turns declarative agent bundles (agent-card.json + prompt.md +
// a config binding) into running ADK agents, each exposed over A2A so the
// orchestrator dispatches to it as a client.
//
// Agents are co-located in-process for now: each gets an ephemeral loopback
// A2A server, so promoting one to a standalone service later is a config swap
// (stable address + HTTP AgentCardProvider), not an agent-code change.
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
)

// invokePath is where each agent's A2A JSON-RPC endpoint is mounted.
const invokePath = "/invoke"

// agentWithWorker is satisfied by wrapper agents (e.g. the trust gate) that
// want their inner worker's tool-derived skills reflected in the published
// AgentCard rather than the wrapper's own (usually empty) skill set.
type agentWithWorker interface {
	Worker() adkagent.Agent
}

// A2AServer is a co-located A2A server exposing one ADK agent over an ephemeral
// loopback port. It owns the listener; Close stops it.
type A2AServer struct {
	// Card is the published AgentCard, with its interface URL pointing at the
	// bound loopback address. Hand it to Client (in-process) - no HTTP
	// resolution needed while co-located.
	Card     *a2a.AgentCard
	listener net.Listener
}

// Serve starts an A2A server for ag on 127.0.0.1:<ephemeral> and returns it with
// the published AgentCard. Session state lives in the shared durable session
// service under ag.Name(), separate from the orchestrator's own sessions and
// surviving a process restart for multi-turn dispatch. mem may be nil (memory
// disabled); the runner tolerates a nil MemoryService.
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

// buildSkills returns the A2A skills for ag. If ag wraps a worker (agentWithWorker),
// the worker's tool-derived skills are used so the published card reflects the
// actual capabilities rather than the wrapper's empty skill set.
func buildSkills(ag adkagent.Agent) []a2a.AgentSkill {
	if w, ok := ag.(agentWithWorker); ok {
		return adka2a.BuildAgentSkills(w.Worker())
	}
	return adka2a.BuildAgentSkills(ag)
}

// Client returns an ADK agent that dispatches to this server over A2A, with
// Name matching the served agent's so transfer-to-agent targets it correctly.
//
// No hard HTTP timeout: context cancellation is the deadline. The default
// a2aclient 3-minute timeout fires mid-judge on long vetting runs.
//
// RemoteTaskCleanupCallback is a no-op - co-located agents share process
// context, so cancellation propagates without an HTTP cancel; the default
// CancelTask call reliably times out under the executor's per-session lock.
func (s *A2AServer) Client() (adkagent.Agent, error) {
	base, err := s.clientNamed(s.Card.Name)
	if err != nil {
		return nil, err
	}
	return nodeClient{Agent: base, srv: s}, nil
}

// nodeClient is the A2A client agent for one server, with the ability to mint a
// per-node CLIENT identity (ForNode). It behaves exactly like the plain client
// otherwise (Name/Description are the served agent's), so the planner roster,
// routing by agent name and the config.agents keys are untouched.
type nodeClient struct {
	adkagent.Agent
	srv *A2AServer
}

// ForNode returns a client agent for the SAME A2A server whose LOCAL identity -
// the Author ADK stamps on every event this client writes - is unique to nodeKey.
//
// Upstream ADK bug: remoteagent picks which remote session to CONTINUE by
// scanning the shared plan session backward for the first event authored
// ctx.Agent().Name() and reusing its contextID. Since concurrent nodes running
// the same agent share one workflow session, that name-keyed scan can match a
// SIBLING node's event, adopting its remote session (and truncating this node's
// own prompt out of the message). A per-node client name is the workaround; the
// server and its AgentCard stay ONE per agent.
//
// nodeKey must be STABLE across a node's judge/revise rounds and HITL
// pause/resume so it can keep resuming its own remote session.
func (c nodeClient) ForNode(nodeKey string) (adkagent.Agent, error) {
	return c.srv.clientNamed(c.srv.Card.Name + "#" + nodeKey)
}

// ClientForNode is Client() with the per-node local CLIENT identity fix
// (see nodeClient.ForNode) already applied - what a per-node-constructed
// server should hand out. Needed even for an otherwise-exclusive server: the
// scan nodeClient.ForNode works around is keyed on the CALLING side's shared
// session, not this server, so two sibling nodes with an unqualified local
// name can still cross-adopt each other's remote session.
func (s *A2AServer) ClientForNode(nodeKey string) (adkagent.Agent, error) {
	return s.clientNamed(s.Card.Name + "#" + nodeKey)
}

// clientNamed builds a remote agent for this server under the given local name.
func (s *A2AServer) clientNamed(name string) (adkagent.Agent, error) {
	factory := a2aclient.NewFactory(
		a2aclient.WithJSONRPCTransport(&http.Client{}),
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

// scopedClient is the A2A client with ONE addition: it rebuilds the outbound
// message's parts from session events belonging to THIS invocation and THIS
// branch, dropping a concurrently-running sibling node's events.
//
// Why here and not in the part converter: remoteagent's history sweep
// re-wraps foreign-authored events via presentAsUserMessage - a synthetic
// event with NO Branch - before the converter ever sees them, so a branch
// check there can't fire. A BeforeRequestCallback can't help either (ADK's
// callback context refuses Session()/Agent()). The client is the one seam
// that sees both the assembled message and the live InvocationContext.
type scopedClient struct{ remoteagent.A2AClient }

func (c scopedClient) SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	scopeMessage(ctx, req)
	return c.A2AClient.SendMessage(ctx, req)
}

func (c scopedClient) SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	scopeMessage(ctx, req)
	return c.A2AClient.SendStreamingMessage(ctx, req)
}

// scopeMessage rewrites req's parts in place. It never touches TaskID/ContextID -
// WHICH remote session to continue stays ADK's call (and, with the per-node client
// identity from ForNode, a correct one).
func scopeMessage(ctx context.Context, req *a2a.SendMessageRequest) {
	ic, ok := ctx.(adkagent.InvocationContext)
	if !ok || req == nil || req.Message == nil || ic.Session() == nil {
		return
	}
	events := ic.Session().Events()
	// The same pivot ADK's sweep uses: everything after this agent's own last
	// response is what the remote session has not seen yet. The per-node client
	// name makes "its own" exact (see nodeClient.ForNode).
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
		// Scoping to the current invocation on top of the branch keeps a previous
		// chat turn's events out of a node's first dispatch (node IDs - hence branch
		// names - repeat across plans). The branch check is THE sibling filter, and
		// it works here because ev is the REAL event.
		if ev == nil || ev.Content == nil || ev.InvocationID != ic.InvocationID() || !eventBelongsToBranch(ic.Branch(), ev) {
			continue
		}
		// Foreign-authored (neither the human nor this agent): describe it, exactly
		// as remoteagent's presentAsUserMessage would - the worker's own prompt
		// arrives this way ("[quack-gate] said: Your task: …"), and no unpaired
		// FunctionCall/Response ever crosses the wire.
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

// describeEvent renders a foreign-authored event as descriptive user-facing text,
// mirroring remoteagent's presentAsUserMessage (ADK v2.0.0 utils.go).
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
			// The gate's prompt-delivery event is authored "quack-gate" (foreign),
			// so a user's attached image/audio rides here as an InlineData part.
			// It has no textual rendering - carry it across the wire verbatim as a
			// raw file part, or the vision/audio model never sees it (media-reader
			// answered "I cannot see the attached image"). Mirrors ADK's own
			// presentAsUserMessage, whose else-branch keeps such parts as-is.
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

// sanitizeWorkflowPlumbingPart is a remoteagent.A2AConfig.GenAIPartConverter
// that neutralizes a node-level HITL/confirm pause's resume plumbing (a
// workflow.WorkflowInputFunctionCallName FunctionCall/Response - see
// vetting/confirm.go's confirmInterruptID and nativegraph.go's
// workflowInputResponses) before it crosses the A2A wire.
//
// Root cause: remoteagent's history sweep renders every non-user,
// non-remote-agent event as descriptive text, but passes "user"-authored
// events through VERBATIM. The human's resume answer is user-authored and
// crosses raw, but the FunctionCall it answers is authored by the plan-graph
// wrapper and gets textified - so a raw FunctionResponse arrives with no
// paired FunctionCall, which the remote server's content builder requires;
// ADK swallows that error into a silent empty completion instead of
// surfacing it.
//
// Fix: recognize the same marker by Name and render it as descriptive text
// ourselves, mirroring what the sweep already does for the request half.
// Only this SERIALIZED-FOR-A2A copy is touched - the shared session's real
// FunctionCall/FunctionResponse events (which the gate scans) stay intact.
func sanitizeWorkflowPlumbingPart(ctx context.Context, adkEvent *session.Event, part *genai.Part) (*a2a.Part, error) {
	if part == nil {
		return nil, nil
	}
	// Branch hygiene: drop events from a DIFFERENT branch of the shared
	// workflow session. remoteagent's history sweep sends the remote worker
	// EVERY event since its own last response with no branch filtering, so a
	// sibling node's gate prompt and relayed activity would otherwise
	// contaminate this node's request (the A2A twin of the local-llmagent leak
	// fixed in vetting/node.go, and the orchestrator-side leak fixed in
	// orchestrator/sessionfilter.go). Apply ADK's own eventBelongsToBranch
	// rule: keep branchless/current-branch/ancestor events, drop the rest -
	// each run's gate prompt is self-contained, so nothing is lost.
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

// eventBelongsToBranch REPRODUCES ADK's own branch-visibility rule: an event
// is visible when either side is branchless, the branches match exactly, or
// the event's branch is a dot-DELIMITED ancestor of the current one (the "."
// is what stops agent_0 from matching agent_00).
//
// ADK's LOCAL LLM flow implements this rule; its A2A flow does not - it sweeps
// every event since the agent's own last response with no branch filter. We
// deliberately apply ADK's own semantics on the path where ADK forgot them.
func eventBelongsToBranch(invocationBranch string, ev *session.Event) bool {
	if invocationBranch == "" || ev.Branch == "" || ev.Branch == invocationBranch {
		return true
	}
	return strings.HasPrefix(invocationBranch, ev.Branch+".")
}
