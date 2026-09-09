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
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"google.golang.org/genai"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/remoteagent/v2"
	"google.golang.org/adk/v2/artifact"
	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/runner"
	adka2a "google.golang.org/adk/v2/server/adka2a/v2"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/compaction"
	"google.golang.org/adk/v2/workflow"

	"github.com/fagerbergj/quack/internal/httpx"
	"github.com/fagerbergj/quack/internal/stream"
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
// comp.Enabled wires adk/v2's native runner-level compaction here; the zero
// Compaction leaves the runner's Compaction nil and changes nothing.
//
// nodeID/sink let this node re-emit a `compaction` SSE event: neither native
// compaction strategy yields its summary into the runner's event stream (both
// only ever call sessions.AppendEvent, see compactionSessions's doc), so the
// only place to observe one is the session service itself. nodeID is baked in
// per node (internal/serve builds one A2AServer per DAG node, see
// nativeAgent.ForNode), so a fan-out sibling can never misattribute another
// node's compaction. sink nil (no active run, or a caller with no hub, e.g.
// tests) makes this a no-op - Serve itself never touches the hub directly.
//
// artifacts is set on the worker's own RunnerConfig - the DAG/orchestrator
// runners already get one (orchestrator.go, nativegraph.go); leaving a
// worker's nil made ctx.Artifacts() a silent nil in every worker tool/callback
// even though the caller has a live service to give it (#A7).
func Serve(ag adkagent.Agent, sessions session.Service, mem adkmemory.Service, artifacts artifact.Service, comp Compaction, nodeID string, sink func(stream.SSEEvent)) (*A2AServer, error) {
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

	adkComp, err := NativeCompactionConfig(comp)
	if err != nil {
		return nil, fmt.Errorf("agent %q: adk compaction: %w", ag.Name(), err)
	}
	executor := adka2a.NewExecutor(adka2a.ExecutorConfig{
		RunnerConfig: runner.Config{
			AppName:           ag.Name(),
			Agent:             ag,
			SessionService:    compactionSessions{Service: sessions, nodeID: nodeID, sink: sink},
			MemoryService:     mem,
			ArtifactService:   artifacts,
			AutoCreateSession: true,
			Compaction:        adkComp,
		},
		OutputMode: adka2a.OutputArtifactPerEvent,
	})

	mux := http.NewServeMux()
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	mux.Handle(invokePath, a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(executor)))

	// otelhttp extracts the client's traceparent header so the request ctx's
	// span (see clientNamed's transport) continues the caller's trace instead
	// of rooting a fresh one (#1046).
	go func() { _ = http.Serve(listener, otelhttp.NewHandler(mux, "a2a.invoke")) }()

	return &A2AServer{Card: card, listener: listener}, nil
}

// Close stops the A2A server's listener.
func (s *A2AServer) Close() error { return s.listener.Close() }

// compactionSessions re-emits the compaction summaries adk/v2's native
// strategies append to the session store, which the runner's event stream
// never carries: the post-invocation sliding-window pass appends via a
// deferred call it documents as "intentionally not yielded to the caller",
// and the intra-invocation token-threshold pass "emits no events" - both
// only ever call AppendEvent on the configured session.Service.
//
// A straggler landing mid-append can trigger a repair record covering the
// same range, so AppendEvent can rarely fire twice for one compaction.
// ponytail: accepted as a rare duplicate SSE/span rather than tracked and
// deduped; revisit if a duplicate row is actually seen in the feed.
type compactionSessions struct {
	session.Service
	nodeID string
	sink   func(stream.SSEEvent)
}

func (s compactionSessions) AppendEvent(ctx context.Context, sess session.Session, ev *session.Event) error {
	if err := s.Service.AppendEvent(ctx, sess, ev); err != nil {
		return err
	}
	emitCompaction(ctx, s.sink, s.nodeID, ev)
	return nil
}

// maxTranscriptChars sizes adk's summarizer transcript cap from the model's
// context window (0 = unknown, keeps adk's own 200k-char default) so
// compaction keeps engaging past adk's default on large-window models.
func maxTranscriptChars(comp Compaction) int {
	return comp.ContextWindow * charsPerToken
}

// NativeCompactionConfig builds adk/v2's runner-level compaction.Config from
// comp, or nil when compaction is disabled. It reuses quack's own tuned
// summarizer prompt (compactionSystemPrompt + summaryTemplate, see
// compaction_prompts.go) rather than adk's default.
func NativeCompactionConfig(comp Compaction) (*compaction.Config, error) {
	if !comp.Enabled {
		return nil, nil
	}
	if comp.Summarizer == nil {
		return nil, fmt.Errorf("compaction: enabled requires a summarizer model")
	}
	prompt := compactionSystemPrompt + "\n\n" + summaryTemplate + "\n\n" + compaction.ConversationHistoryPlaceholder
	summarizer, err := compaction.NewLLMSummarizer(compaction.LLMSummarizerConfig{
		Model:              comp.Summarizer,
		PromptTemplate:     prompt,
		MaxTranscriptChars: maxTranscriptChars(comp),
		// ADK's 2000-char default (llm_summarizer.go) would cut the rolling
		// summary itself on the way back in; quack's own prompt asks for full
		// technical content and can legitimately run tens of KB.
		MaxToolContentChars: -1,
	})
	if err != nil {
		return nil, err
	}
	cfg := &compaction.Config{
		CompactionInterval: comp.CompactionInterval,
		OverlapSize:        comp.OverlapSize,
		TokenThreshold:     comp.TokenThreshold,
		EventRetentionSize: comp.EventRetentionSize,
		Summarizer:         summarizer,
	}
	// TokenThreshold defaults to 0 (disabled) in adk; quack's own threshold()
	// falls back to the model's context window when unset, so mirror that here
	// rather than silently disabling tail retention.
	if cfg.TokenThreshold == 0 && comp.ContextWindow > 0 {
		cfg.TokenThreshold = usable(comp.ContextWindow)
	}
	if cfg.EventRetentionSize == 0 && cfg.TokenThreshold > 0 {
		cfg.EventRetentionSize = defaultEventRetentionSize
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// buildSkills returns the A2A skills for ag.
func buildSkills(ag adkagent.Agent) []a2a.AgentSkill {
	return adka2a.BuildAgentSkills(ag)
}

// ClientForNode returns an ADK agent that dispatches to this server over A2A,
// under an identity unique to nodeKey (works around an ADK remote-session
// collision bug for concurrent sibling nodes). contextID seeds the A2A
// message's ContextID whenever ADK sends one with none, so the worker
// session this node creates has a deterministic address instead of a
// server-minted random UUID (#A2 - see WorkerSessionID).
func (s *A2AServer) ClientForNode(nodeKey, contextID string) (adkagent.Agent, error) {
	return s.clientNamed(s.Card.Name+"#"+nodeKey, contextID)
}

// WorkerSessionID is the deterministic ADK session id a node's own A2A
// worker session is created under - shared by ClientForNode's caller (to
// derive contextID) and release()'s cleanup (to delete the same row).
func WorkerSessionID(chatID, nodeID string) string { return chatID + ":" + nodeID }

// WorkerSessionUser is the ADK user id a2a-go (server/adka2a/v2/metadata.go)
// derives from a message's ContextID for AutoCreateSession sessions.
func WorkerSessionUser(contextID string) string { return "A2A_USER_" + contextID }

// clientNamed builds a remote agent for this server under the given local name.
func (s *A2AServer) clientNamed(name, contextID string) (adkagent.Agent, error) {
	// otelhttp injects the caller's traceparent header so the per-node A2A
	// server's handler continues this trace instead of starting a new one (#1046).
	factory := a2aclient.NewFactory(
		a2aclient.WithJSONRPCTransport(&http.Client{Transport: httpx.NewTransport(otelhttp.NewTransport(nil))}),
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
			return scopedClient{A2AClient: c, contextID: contextID}, nil
		},
		GenAIPartConverter:        sanitizeWorkflowPlumbingPart,
		RemoteTaskCleanupCallback: func(context.Context, *a2a.AgentCard, remoteagent.A2AClient, a2a.TaskInfo, error) {},
	})
}

// scopedClient filters sibling node events from outbound A2A messages by
// invocation + branch, since the part converter sees already-synthetic events.
type scopedClient struct {
	remoteagent.A2AClient
	contextID string
}

func (c scopedClient) SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	scopeMessage(ctx, req, c.contextID)
	return c.A2AClient.SendMessage(ctx, req)
}

func (c scopedClient) SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	scopeMessage(ctx, req, c.contextID)
	return c.A2AClient.SendStreamingMessage(ctx, req)
}

// scopeMessage rewrites req's parts in place, scoped to invocation + branch,
// and clears its task/context IDs when ADK's own resume scan (branch-blind)
// crossed into a sibling node's event to get them - leaves them alone
// otherwise, since a HITL resume derives its own IDs a different way.
//
// defaultContextID seeds req.Message.ContextID when it arrives empty -
// otherwise a2a-go mints a fresh random UUID per message
// (a2asrv/agentexec.go:createNewExecutionContext) and the worker session it
// addresses (server/adka2a/v2/metadata.go:toInvocationMeta) is never seen
// again, so nothing can ever delete it (#A2).
func scopeMessage(ctx context.Context, req *a2a.SendMessageRequest, defaultContextID string) {
	if req == nil || req.Message == nil {
		return
	}
	ic, ok := ctx.(adkagent.InvocationContext)
	if !ok || ic.Session() == nil {
		if req.Message.ContextID == "" {
			req.Message.ContextID = defaultContextID
		}
		return
	}
	events := ic.Session().Events()
	start := 0
	// crossedBranch: the clear below deliberately wants a fresh ADK-minted
	// context (a HITL resume derives its own IDs a different way), so the
	// empty-ContextID default below must not refill it in that case.
	crossedBranch := false
	for i := events.Len() - 1; i >= 0; i-- {
		if ev := events.At(i); ev != nil && ev.Author == ic.Agent().Name() {
			start = i + 1
			if !eventBelongsToBranch(ic.Branch(), ev) {
				req.Message.TaskID, req.Message.ContextID = "", ""
				crossedBranch = true
			}
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
	if req.Message.ContextID == "" && !crossedBranch {
		req.Message.ContextID = defaultContextID
	}
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
