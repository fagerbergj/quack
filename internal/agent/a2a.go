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
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/google/uuid"
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
	runs     *workerRuns
}

// Serve starts an A2A server for ag on 127.0.0.1:<ephemeral>. nodeID/sink re-emit compaction summaries
// (compactionSessions); nodeID is per node so siblings never misattribute one, and a nil sink is a no-op.
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

	runs := &workerRuns{}
	mux := http.NewServeMux()
	mux.Handle(a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	mux.Handle(invokePath, a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(trackedExecutor{AgentExecutor: executor, runs: runs})))

	// otelhttp extracts the client's traceparent so the request continues the caller's trace.
	go func() { _ = http.Serve(listener, otelhttp.NewHandler(mux, "a2a.invoke")) }()

	return &A2AServer{Card: card, listener: listener, runs: runs}, nil
}

// workerRuns maps a send's runKeyMeta to its in-flight worker. a2a-go runs a worker on a
// detached ctx, so a cancelled client must stop it here or it outlives the node.
type workerRuns struct{ m sync.Map }

type workerRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// stopTimeout bounds how long a cancelled node waits for its worker to stop.
const stopTimeout = 10 * time.Second

// runKeyMeta: request metadata carrying a per-send key; unlike ContextID, a HITL resume never clears it.
const runKeyMeta = "quack_run_key"

// stop cancels key's worker and waits (bounded) for it to exit.
// ponytail: a send cancelled before its Execute registered is missed.
func (w *workerRuns) stop(key string) {
	v, ok := w.m.Load(key)
	if !ok {
		return
	}
	run := v.(*workerRun)
	run.cancel()
	select {
	case <-run.done:
	case <-time.After(stopTimeout):
		slog.Warn("a2a: worker still running after cancel", "component", "agent", "run_key", key)
	}
}

// trackedExecutor registers each execution in runs for the client's stop.
type trackedExecutor struct {
	a2asrv.AgentExecutor
	runs *workerRuns
}

func (t trackedExecutor) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	key, _ := execCtx.Metadata[runKeyMeta].(string)
	if key == "" {
		return t.AgentExecutor.Execute(ctx, execCtx)
	}
	return func(yield func(a2a.Event, error) bool) {
		ctx, cancel := context.WithCancel(ctx)
		run := &workerRun{cancel: cancel, done: make(chan struct{})}
		t.runs.m.Store(key, run)
		defer func() {
			t.runs.m.Delete(key)
			cancel()
			close(run.done)
		}()
		t.AgentExecutor.Execute(ctx, execCtx)(yield)
	}
}

func (s *A2AServer) Close() error { return s.listener.Close() }

// compactionSessions re-emits compaction summaries, which adk's strategies only AppendEvent to the session store.
// ponytail: a mid-append straggler can rarely emit one compaction twice; dedupe if a duplicate row shows up.
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

// maxTranscriptChars sizes adk's summarizer transcript cap from the context window
// (0 = unknown keeps adk's 200k-char default) so compaction keeps engaging on large-window models.
func maxTranscriptChars(comp Compaction) int {
	return comp.ContextWindow * charsPerToken
}

// NativeCompactionConfig builds adk's runner-level compaction.Config with quack's own summarizer prompt,
// or nil when compaction is disabled.
func NativeCompactionConfig(comp Compaction) (*compaction.Config, error) {
	if !comp.Enabled {
		return nil, nil
	}
	if comp.Summarizer == nil {
		return nil, fmt.Errorf("compaction: enabled requires a summarizer model")
	}
	// Runs at node build, off any round's context; a prompt source carries its own deadline.
	sys, tmpl, err := compactionPrompts(context.Background(), comp.Prompts)
	if err != nil {
		return nil, err
	}
	prompt := sys + "\n\n" + tmpl + "\n\n" + compaction.ConversationHistoryPlaceholder
	summarizer, serr := compaction.NewLLMSummarizer(compaction.LLMSummarizerConfig{
		Model:              comp.Summarizer,
		PromptTemplate:     prompt,
		MaxTranscriptChars: maxTranscriptChars(comp),
		// ADK's 2000-char default would cut the rolling summary itself, which can legitimately run tens of KB.
		MaxToolContentChars: -1,
	})
	if serr != nil {
		return nil, serr
	}
	cfg := &compaction.Config{
		CompactionInterval: comp.CompactionInterval,
		OverlapSize:        comp.OverlapSize,
		TokenThreshold:     comp.TokenThreshold,
		EventRetentionSize: comp.EventRetentionSize,
		Summarizer:         summarizer,
	}
	// adk treats 0 as disabled; mirror quack's threshold() fallback to the context window.
	if cfg.TokenThreshold == 0 && comp.ContextWindow > 0 {
		cfg.TokenThreshold = usable(comp.ContextWindow)
	}
	if cfg.EventRetentionSize == 0 && cfg.TokenThreshold > 0 {
		cfg.EventRetentionSize = defaultEventRetentionSize
	}
	if comp.Meter != nil && comp.Meter.resolvable && cfg.TokenThreshold > 0 {
		cfg.Summarizer = collapsingSummarizer{inner: summarizer, meter: comp.Meter, threshold: cfg.TokenThreshold}
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func buildSkills(ag adkagent.Agent) []a2a.AgentSkill {
	return adka2a.BuildAgentSkills(ag)
}

// ClientForNode dispatches over A2A under a name unique to nodeKey, working around ADK's remote-session collision for
// concurrent siblings. contextID fills an empty ContextID so the worker session has a deterministic address.
func (s *A2AServer) ClientForNode(nodeKey, contextID string) (adkagent.Agent, error) {
	return s.clientNamed(s.Card.Name+"#"+nodeKey, contextID)
}

// WorkerSessionID is a node's deterministic worker session id, shared by ClientForNode's caller and release().
func WorkerSessionID(chatID, nodeID string) string { return chatID + ":" + nodeID }

// WorkerSessionUser is the ADK user id a2a-go (server/adka2a/v2/metadata.go)
// derives from a message's ContextID for AutoCreateSession sessions.
func WorkerSessionUser(contextID string) string { return "A2A_USER_" + contextID }

// clientNamed builds a remote agent for this server under the given local name.
func (s *A2AServer) clientNamed(name, contextID string) (adkagent.Agent, error) {
	// otelhttp injects traceparent so the per-node A2A server continues this trace.
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
			return scopedClient{A2AClient: c, contextID: contextID, runs: s.runs}, nil
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
	runs      *workerRuns
}

func (c scopedClient) SendMessage(ctx context.Context, req *a2a.SendMessageRequest) (a2a.SendMessageResult, error) {
	scopeMessage(ctx, req, c.contextID)
	defer c.stopIfCancelled(ctx, tagRun(req))
	return c.A2AClient.SendMessage(ctx, req)
}

func (c scopedClient) SendStreamingMessage(ctx context.Context, req *a2a.SendMessageRequest) iter.Seq2[a2a.Event, error] {
	scopeMessage(ctx, req, c.contextID)
	key := tagRun(req)
	return func(yield func(a2a.Event, error) bool) {
		defer c.stopIfCancelled(ctx, key)
		for ev, err := range c.A2AClient.SendStreamingMessage(ctx, req) {
			if !yield(ev, err) {
				return
			}
		}
	}
}

// stopIfCancelled keeps a cancelled node's worker from outliving the node's run.
func (c scopedClient) stopIfCancelled(ctx context.Context, key string) {
	if ctx.Err() != nil && c.runs != nil {
		c.runs.stop(key)
	}
}

// tagRun stamps req with a fresh run key for trackedExecutor and returns it.
func tagRun(req *a2a.SendMessageRequest) string {
	key := uuid.NewString()
	if req.Metadata == nil {
		req.Metadata = map[string]any{}
	}
	req.Metadata[runKeyMeta] = key
	return key
}

// scopeMessage scopes req's parts to invocation + branch and clears task/context IDs taken from a sibling's event.
// defaultContextID fills an empty ContextID; otherwise a2a-go mints a random one and the worker session leaks.
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
	// crossedBranch: the clear below wants a fresh ADK-minted context (HITL
	// resumes derive IDs differently), so the default must not refill it.
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
	req.Message.Parts = collectScopedParts(ic, events, start, len(req.Message.Parts))
	if req.Message.ContextID == "" && !crossedBranch {
		req.Message.ContextID = defaultContextID
	}
}

// collectScopedParts: the branch's events from start as A2A parts - foreign authors
// via describeEvent, own/user parts with plumbing sanitized or dropped.
func collectScopedParts(ic adkagent.InvocationContext, events session.Events, start, cap int) []*a2a.Part {
	parts := make([]*a2a.Part, 0, cap)
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
	return parts
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

// sanitizeWorkflowPlumbingPart neutralizes HITL/resume FunctionCall/Response pairs before they cross the A2A wire;
// mismatched pairs otherwise cause silent empty completions on the remote server.
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
