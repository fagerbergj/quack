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
// mem is the semantic-memory service made available to the agent's runtime (so
// its preload_memory / load_memory tools resolve ctx.SearchMemory). Pass nil
// when memory is disabled — the runner tolerates a nil MemoryService and the
// agent simply carries no memory tools.
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

// Client returns an ADK agent that dispatches to this server over A2A. Use it as
// a sub-agent of the orchestrator; its Name matches the served agent's, so
// transfer-to-agent targets it correctly.
//
// The HTTP client has no hard timeout — context cancellation is the deadline.
// The default a2aclient factory applies a 3-minute total-request timeout which
// fires mid-judge on long vetting runs (worker + self-refine already consume
// most of those 3 minutes before the judge even starts).
//
// RemoteTaskCleanupCallback is a no-op: co-located agents share the same
// process context, so cancellation propagates naturally through the goroutine
// tree without an explicit HTTP cancel request. The default cleanup sends a
// CancelTask HTTP call that reliably times out (the A2A executor holds a
// per-session lock while running), producing a spurious WARN on every client
// disconnect.
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

// ForNode returns a client agent for the SAME A2A server whose LOCAL identity —
// the Name ADK stamps as the Author of every event this client writes into the
// shared plan session — is unique to nodeKey.
//
// Why: remoteagent decides which remote A2A session to CONTINUE by scanning the
// local session backward for the first event whose Author == ctx.Agent().Name()
// and reusing that event's A2A contextID (ADK v2.0.0
// agent/remoteagent/v2/utils.go, toMissingRemoteSessionParts). Every plan node
// shares ONE workflow session, so when several concurrent nodes run the SAME
// agent, that author-keyed scan matches a SIBLING node's event and the node
// adopts the sibling's remote session — inheriting its task and history (live:
// five concurrent code-explorer nodes, and the OpenHands node cloned goose's
// repo). It also truncates the node's own prompt out of the outbound message,
// which is how two nodes lost their task entirely and asked the user which repo
// to explore. This is an upstream ADK bug: the lookup keys on a name that is not
// unique per invocation. A per-node client name is the client-side workaround —
// the server (and its published AgentCard) stays ONE per agent.
//
// nodeKey must be STABLE for a node across its judge/revise rounds and across a
// HITL pause/resume — that stability is what lets the node resume its own remote
// session (multi-turn dispatch).
func (c nodeClient) ForNode(nodeKey string) (adkagent.Agent, error) {
	return c.srv.clientNamed(c.srv.Card.Name + "#" + nodeKey)
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
// message's parts from the session events that belong to THIS invocation and
// THIS branch, dropping a concurrently-running sibling node's events.
//
// Why here and not in the part converter (where the branch filter used to live):
// remoteagent's history sweep (toMissingRemoteSessionParts) hands FOREIGN-authored
// events to the converter only AFTER re-wrapping them via presentAsUserMessage — a
// synthetic event built with session.NewEvent, which carries NO Branch. By the time
// the converter sees a sibling node's event its branch is gone, so a branch check
// there cannot fire. Nor can a BeforeRequestCallback help: ADK's callback context
// refuses Session() and Agent(). The client is the one seam that still sees BOTH
// the assembled message AND the live InvocationContext (remoteagent passes its
// InvocationContext straight into SendMessage/SendStreamingMessage).
//
// Live consequence of the gap: every plan node shares ONE workflow session, so a
// concurrent sibling's gate prompt (the event that carries its TASK) and its
// relayed tool activity were textified into this node's outbound message — "For
// context: [quack-gate] said: Your task: <the sibling's task>" — and the OpenHands
// explorer cloned goose's repo.
type scopedClient struct{ remoteagent.A2AClient }

func (c scopedClient) SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	scopeMessage(ctx, req)
	return c.A2AClient.SendMessage(ctx, req)
}

func (c scopedClient) SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	scopeMessage(ctx, req)
	return c.A2AClient.SendStreamingMessage(ctx, req)
}

// scopeMessage rewrites req's parts in place. It never touches TaskID/ContextID —
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
		// chat turn's events out of a node's first dispatch (node IDs — hence branch
		// names — repeat across plans). The branch check is THE sibling filter, and
		// it works here because ev is the REAL event.
		if ev == nil || ev.Content == nil || ev.InvocationID != ic.InvocationID() || !eventBelongsToBranch(ic.Branch(), ev) {
			continue
		}
		// Foreign-authored (neither the human nor this agent): describe it, exactly
		// as remoteagent's presentAsUserMessage would — the worker's own prompt
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
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return append([]*a2a.Part{a2a.NewTextPart("For context:")}, parts...)
}

// sanitizeWorkflowPlumbingPart is a remoteagent.A2AConfig.GenAIPartConverter
// that neutralizes a node-level HITL/confirm pause's own resume plumbing (a
// workflow.WorkflowInputFunctionCallName "adk_request_input" FunctionCall or
// FunctionResponse — see vetting/confirm.go's confirmInterruptID and
// nativegraph.go's workflowInputResponses) before it crosses the A2A wire.
//
// Root cause (live bug: an approved confirm/HITL decision resumed over A2A
// never reached the worker — the node just went silently empty): remoteagent's
// own history sweep (toMissingRemoteSessionParts) renders every event NOT
// authored "user" or by the remote agent itself as descriptive text, but
// passes a "user"-authored event through VERBATIM. The human's resume answer
// IS delivered as a "user"-authored event (correctly — it is genuinely the
// human's turn), while the ORIGINAL adk_request_input FunctionCall it answers
// is authored by the plan-graph wrapper — neither "user" nor the remote agent
// — so THAT event gets textified. The result: a raw FunctionResponse crosses
// the wire with no FunctionCall anywhere in the swept history to pair it to.
// The remote server's own content builder requires that pairing for the most
// recent response and errors ("no function call event found for function
// responses ids") when it can't find one; ADK's flow machinery swallows that
// error into a silent empty completion rather than surfacing it, so the
// worker never re-runs at all and the gate's empty-answer recovery kicks in
// with no writer model configured.
//
// Fix: recognize the same marker by FunctionCall/FunctionResponse Name and
// render it as descriptive text ourselves — exactly what the sweep already
// does for the request half of the pair — so no orphaned function part ever
// reaches the wire. Only this session's SERIALIZED-FOR-A2A copy is affected;
// the shared session quack's own gate scans (scanNodeConfirms/scanNodeAsks)
// keeps the real FunctionCall/FunctionResponse events untouched.
func sanitizeWorkflowPlumbingPart(ctx context.Context, adkEvent *session.Event, part *genai.Part) (*a2a.Part, error) {
	if part == nil {
		return nil, nil
	}
	// Branch hygiene: drop events that belong to a DIFFERENT branch of the
	// shared workflow session. Every plan node runs in ONE session, and
	// remoteagent's history sweep (toMissingRemoteSessionParts) sends the remote
	// worker EVERY event since its own last response with NO branch filtering —
	// so a concurrently-running sibling node's gate prompt, ask/confirm plumbing,
	// and relayed worker activity would all be textified ("For context: …") into
	// this node's outbound message: one node's task contaminating another's
	// request (the A2A twin of the local-llmagent leak fixed by the worker-run
	// isolation scope in vetting/node.go, and of the orchestrator-side leak
	// fixed by orchestrator/sessionfilter.go). The converter is the one seam
	// quack controls on this path, and ctx here is the remote agent's
	// InvocationContext, which carries the current worker run's branch — apply
	// ADK's own eventBelongsToBranch rule: keep branchless (invocation-shared)
	// events, the current branch, and ancestors; drop everything else (sibling
	// nodes, this node's own earlier runs — each run's gate prompt is
	// self-contained, so nothing is lost).
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

// eventBelongsToBranch REPRODUCES ADK's own branch-visibility rule, rule for
// rule (adk/v2 internal/llminternal/contents_processor.go:205,
// eventBelongsToBranch): an event is visible to the current branch when either
// side is branchless, the branches match exactly, or the event's branch is a
// dot-DELIMITED ancestor of the current one (the explicit "." is what stops
// agent_0 from matching agent_00).
//
// This is ADK's intended design for parallel work — workflow/scheduler.go:288:
// "Branch scopes LLM history visibility (via the flow processor's branch-prefix
// filter) and gets stamped onto every emitted event when the node leaves
// Event.Branch empty." ADK's LOCAL LLM flow implements it. ADK's A2A flow
// (agent/remoteagent/v2/utils.go, toMissingRemoteSessionParts) does NOT — it
// sweeps every event since the agent's own last response with no branch filter.
// We deliberately apply ADK's semantics on the path where ADK forgot them.
func eventBelongsToBranch(invocationBranch string, ev *session.Event) bool {
	if invocationBranch == "" || ev.Branch == "" || ev.Branch == invocationBranch {
		return true
	}
	return strings.HasPrefix(invocationBranch, ev.Branch+".")
}
