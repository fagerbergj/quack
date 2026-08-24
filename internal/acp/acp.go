// Package acp runs an external coding agent as an ACP subprocess adapted to an
// ADK agent, one subprocess per worker round.
// ponytail: process-per-round re-reads context each round - keep the process
// alive per node if round startup ever matters.
package acp

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	"go.opentelemetry.io/otel/attribute"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/replay"
	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// Options configures one external ACP agent.
type Options struct {
	Command []string // argv to spawn, e.g. the pi-acp shim: ["node", "/usr/local/lib/pi-acp/pi-acp.mjs"]
	Env     []string
	Caps    workspace.Caps
	ExtraRO []string
	Home    string
	// ponytail: computed once, not per round, so its date footer can go stale
	// on a long-lived server; raise it to a func() string if that ever bites.
	Preamble        string
	Jail            *workspace.Jail
	UserID          string
	Worktree        func(ctx context.Context, userID, chatID, parentNodeID, nodeID string) (dir string, err error)
	StartTimeout    time.Duration
	IdleTimeout     time.Duration
	PermissionJudge func(ctx context.Context, toolName, title string, input map[string]any) (allow bool, reason string)
	Replay          *replay.Session
	// ModelName: the model this agent's opencode config binds it to
	// (OPENCODE_CONFIG_CONTENT) - attrs the round's gen_ai metrics.
	ModelName string
	// Pricing: nil = no price table entry for ModelName, cost metric skipped.
	Pricing *config.ModelPricing
	// RegisterLiveSteer/UnregisterLiveSteer let a queued message land
	// mid-round instead of at the next gate boundary (#998). nil = park always.
	RegisterLiveSteer   func(chatID, nodeID string, forward func(text string) bool)
	UnregisterLiveSteer func(chatID, nodeID string)
	// RegisterRoundAbort/UnregisterRoundAbort let CancelNode reach a running
	// round's abort RPC directly instead of waiting for the round to end
	// (#1030). Cancel only - never wired for pause, which must preserve
	// whatever the round has accumulated so it can resume.
	RegisterRoundAbort   func(chatID, nodeID string, cancel context.CancelFunc)
	UnregisterRoundAbort func(chatID, nodeID string)
}

// Agent is an adkagent.Agent backed by an external ACP subprocess.
type Agent struct {
	adkagent.Agent
	name string
	opts Options
	log  *slog.Logger

	mu     sync.Mutex
	coords ledger.Coords
}

// SetLedgerCoords stamps coordinates for the next round - copied into a
// local at round start (round() below), not read live, since this Agent is
// shared across concurrent nodes and a round can run for many minutes.
func (a *Agent) SetLedgerCoords(c ledger.Coords) {
	a.mu.Lock()
	a.coords = c
	a.mu.Unlock()
}

// New builds an ACP-backed agent.
func New(name, description string, opts Options) (*Agent, error) {
	if len(opts.Command) == 0 {
		return nil, errors.New("acp: empty command")
	}
	if opts.Jail == nil {
		return nil, errors.New("acp: no workspace jail configured")
	}
	if opts.StartTimeout <= 0 {
		opts.StartTimeout = 60 * time.Second
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 10 * time.Minute
	}
	a := &Agent{name: name, opts: opts, log: slog.With("component", "acp", "agent", name)}
	inner, err := adkagent.New(adkagent.Config{
		Name:        name,
		Description: description,
		Run:         a.run,
	})
	if err != nil {
		return nil, fmt.Errorf("acp: %w", err)
	}
	a.Agent = inner
	return a, nil
}

// run is the plain-agent path for Run outside a workflow node.
func (a *Agent) run(ic adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
	return a.runPrompt(ic, contentText(ic.UserContent()))
}

// RunNode is the node-runner path the gate's RunNode drives (vetting
// runWorkerNode): nodeInput is the fully-assembled round prompt.
func (a *Agent) RunNode(ctx adkagent.Context, nodeInput any) iter.Seq2[*session.Event, error] {
	return a.runPrompt(ctx, inputText(nodeInput))
}

// resolveNode derives the node's working directory, memory-MCP credential,
// GitHub context-dir grant, and per-node scratch dir from the advisor-thread
// marker in the prompt. memSecret is resolved separately in the memSessions
// registry - the advisor-thread token never doubles as the MCP bearer
// credential. chatID/nodeID are the advisor thread's own (at.ChatID,
// at.NodeID) - the executor's controls key (dag's controls.register(chatID,
// node.ID)), unlike cfg.NodeID (which collapses to the shared workspace scope
// on a setup chain's writer node) or at.SessionID (the ADK session id, a
// retry-only alias - see AdvisorTask.ChatID).
func (a *Agent) resolveNode(ctx context.Context, prompt string) (cwd, memSecret, ctxDir, scratchDir string, readOnly bool, chatID, nodeID string, err error) {
	token, ok := vetting.ParseAdvisorThread(prompt)
	if !ok {
		return "", "", "", "", false, "", "", errors.New("acp: prompt carries no workspace-scope marker (is this agent running outside the gate?)")
	}
	at, ok := vetting.LookupAdvisorThread(token)
	if !ok {
		return "", "", "", "", false, "", "", fmt.Errorf("acp: advisor thread %q not registered", token)
	}
	chatID, nodeID = at.ChatID, at.NodeID
	if a.opts.Jail != nil {
		ctxDir, _ = a.opts.Jail.Resolve(a.opts.UserID, at.ChatID, workspace.ContextDirScope)
		// A read-only reviewer needs this exactly as much as a writer does
		// (TMPDIR/mktemp/heredocs don't care whether the round can touch its
		// own tree) - scoped per node so concurrent rounds never collide.
		scratchDir, err = a.opts.Jail.ScratchDir(a.opts.UserID, at.ChatID, at.WorkspaceNodeID)
		if err != nil {
			return "", "", "", "", false, chatID, nodeID, fmt.Errorf("acp: scratch dir: %w", err)
		}
	}
	if at.WorktreeParent != "" {
		if a.opts.Worktree == nil {
			return "", "", "", "", false, chatID, nodeID, fmt.Errorf("acp: node %q needs a git worktree but no worktree executor is configured", at.NodeID)
		}
		cwd, err = a.opts.Worktree(ctx, a.opts.UserID, at.ChatID, at.WorktreeParent, at.WorkspaceNodeID)
		return cwd, at.MemSecret, ctxDir, scratchDir, at.ReadOnly, chatID, nodeID, err
	}
	cwd, err = a.opts.Jail.EnsureDir(a.opts.UserID, at.ChatID, workspace.NodeDir(at.WorkspaceNodeID))
	return cwd, at.MemSecret, ctxDir, scratchDir, at.ReadOnly, chatID, nodeID, err
}

// runPrompt is one full round: spawn, handshake, prompt, stream translation, shutdown.
func (a *Agent) runPrompt(ctx adkagent.InvocationContext, prompt string) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		if strings.TrimSpace(prompt) == "" {
			yield(nil, errors.New("acp: empty prompt"))
			return
		}
		cwd, memSecret, ctxDir, scratchDir, readOnly, steerChatID, steerNodeID, err := a.resolveNode(ctx, prompt)
		if err != nil {
			yield(nil, err)
			return
		}
		// Per-round scratch (the child's TMPDIR) is recreated on demand by
		// homeTmpDir at the next spawn; removing it here keeps a run from
		// leaving build tmp files around until the gc TTL sweep.
		if scratchDir != "" {
			defer os.RemoveAll(scratchDir)
		}
		// caps.ReadOnly comes from THIS node's advisor task, not the agent's
		// static config - a planOnly run forces it true per-node (#754/#739)
		// regardless of what the agent is normally configured for.
		caps := a.opts.Caps
		caps.ReadOnly = readOnly
		caps.ScratchDir = scratchDir
		// Environment block goes AFTER the task: it is regenerated every round
		// (branch/HEAD/dir listing drift once a round commits anything), so
		// leading with it broke the prompt-cache prefix from round 2 on.
		outbound := prompt + "\n\n" + environmentBlock(ctx, cwd, caps)
		if a.opts.Preamble != "" {
			outbound = a.opts.Preamble + "\n\n" + outbound
		}
		var extraRO []string
		if ctxDir != "" {
			extraRO = []string{ctxDir}
		}
		stopped := false
		err = a.round(ctx, cwd, memSecret, extraRO, caps, outbound, steerChatID, steerNodeID, func(spec eventSpec) bool {
			if !yield(a.newEvent(ctx, spec), nil) {
				stopped = true
				return false
			}
			return true
		})
		if err != nil && !stopped {
			yield(nil, err)
		}
	}
}

// steerExtMethod: ACP extension (#998) forwarding a mid-round steer to the shim.
const steerExtMethod = "_quack/steer"

type steerParams struct {
	Text string `json:"text"`
}

// promptDone carries the Prompt RPC's outcome off the goroutine in round.
type promptDone struct {
	resp sdk.PromptResponse
	err  error
}

// round drives one subprocess round. Separated from runPrompt for testability.
// caps is the node's EFFECTIVE caps (ReadOnly already resolved by the
// caller) - the one thing that can legitimately differ per round for an
// otherwise-static agent (#754).
// steerChatID/steerNodeID key the live-steer hook: the advisor thread's
// SessionID/NodeID (round()'s callers resolve these), NOT ledger.Coords -
// cfg.NodeID collapses to the shared workspace scope for a setup-chain's
// writer node, which would silently no-op the hook (#998 review).
func (a *Agent) round(ctx context.Context, cwd, memSecret string, extraRO []string, caps workspace.Caps, outbound string, steerChatID, steerNodeID string, emit func(eventSpec) bool) (err error) {
	ctx, roundSpan := otelobs.Start(ctx, "acp.round", attribute.String(otelobs.GenAIAgentName, a.name), attribute.String("cwd", cwd))
	defer func() { otelobs.End(roundSpan, err) }()

	// Snapshot now, before any subprocess I/O - see SetLedgerCoords.
	a.mu.Lock()
	coords := a.coords
	a.mu.Unlock()

	// abortCtx is CancelNode's direct line into this round (#1030), separate
	// from ctx (which also carries parent shutdown) so both trigger the same
	// graceful-cancel path below without one masking the other's cause.
	abortCtx, abortCancel := context.WithCancel(context.Background())
	defer abortCancel()
	if a.opts.RegisterRoundAbort != nil && steerChatID != "" && steerNodeID != "" {
		a.opts.RegisterRoundAbort(steerChatID, steerNodeID, abortCancel)
		if a.opts.UnregisterRoundAbort != nil {
			defer a.opts.UnregisterRoundAbort(steerChatID, steerNodeID)
		}
	}

	spawnCtx, spawnSpan := otelobs.Start(ctx, "acp.spawn", attribute.String(otelobs.GenAIAgentName, a.name))
	_ = spawnCtx
	h, err := a.start(ctx, cwd, extraRO, caps)
	otelobs.End(spawnSpan, err)
	if err != nil {
		return err
	}
	defer h.close(a.log)
	defer func() { emitInvokeAgent(ctx, a.name, h.sent, h.received, err) }()

	ictx, cancelInit := context.WithTimeout(ctx, a.opts.StartTimeout)
	defer cancelInit()
	handshakeCtx, handshakeSpan := otelobs.Start(ctx, "acp.handshake", attribute.String(otelobs.GenAIAgentName, a.name))
	_ = handshakeCtx
	initResp, err := h.conn.Initialize(ictx, sdk.InitializeRequest{
		ProtocolVersion:    sdk.ProtocolVersionNumber,
		ClientCapabilities: sdk.ClientCapabilities{},
	})
	if err != nil {
		otelobs.End(handshakeSpan, err)
		return fmt.Errorf("acp: initialize: %w%s", err, h.stderrTail())
	}
	mcpServers := memoryMCPServers(memSecret, initResp.AgentCapabilities)
	memSession, _ := vetting.LookupMemSession(memSecret)
	toolNames := mcpToolNames(memSession, len(mcpServers) > 0)
	a.log.Info("acp negotiated capabilities", "mcp_http", initResp.AgentCapabilities.McpCapabilities.Http,
		"mcp_sse", initResp.AgentCapabilities.McpCapabilities.Sse, "mcp_acp", initResp.AgentCapabilities.McpCapabilities.Acp,
		"mcp_surface_offered", len(mcpServers) > 0, "has_mem_secret", memSecret != "", "mcp_tools", toolNames)
	sess, err := h.conn.NewSession(ictx, sdk.NewSessionRequest{Cwd: cwd, McpServers: mcpServers})
	if err != nil {
		otelobs.End(handshakeSpan, err)
		return fmt.Errorf("acp: session/new: %w%s", err, h.stderrTail())
	}
	handshakeSpan.SetAttributes(attribute.String("session_id", string(sess.SessionId)))
	otelobs.End(handshakeSpan, nil)
	a.log.Info("acp round started", "cwd", cwd, "session", sess.SessionId)

	// Live only for this round's duration - nothing to forward into before/after.
	// CallExtension (an acked request), not NotifyExtension: between the
	// shim settling and the deferred Unregister below the connection is
	// still open, so a fire-and-forget notify would report delivered while
	// the shim silently drops it (promptReq already nil). A failed/errored
	// call reports false, and enqueue's caller parks it instead (#998 review).
	if a.opts.RegisterLiveSteer != nil && steerChatID != "" && steerNodeID != "" {
		conn := h.conn
		a.opts.RegisterLiveSteer(steerChatID, steerNodeID, func(text string) bool {
			_, err := conn.CallExtension(context.Background(), steerExtMethod, steerParams{Text: text})
			return err == nil
		})
		if a.opts.UnregisterLiveSteer != nil {
			defer a.opts.UnregisterLiveSteer(steerChatID, steerNodeID)
		}
	}

	finalPrompt := mcpToolsBlock(toolNames) + "\n\n" + outbound

	done := make(chan promptDone, 1)
	promptCtx, promptSpan := otelobs.Start(ctx, "acp.prompt", attribute.String(otelobs.GenAIAgentName, a.name), attribute.String("session_id", string(sess.SessionId)))
	defer promptSpan.End() // safety net for the relay-stopped/cancel exits below; the done-branch sets the real status first
	// Per-tool-call child spans, ended as their updates arrive - the only
	// telemetry that reaches a collector before the round finishes (#924).
	turns := newTurnSpans(promptCtx, a.name)
	defer turns.closeAll() // LIFO: runs before promptSpan.End() above
	endPrompt := func(err error) {
		turns.closeAll()
		otelobs.End(promptSpan, err)
	}
	go func() {
		resp, perr := h.conn.Prompt(context.Background(), sdk.PromptRequest{
			SessionId: sess.SessionId,
			Prompt:    []sdk.ContentBlock{sdk.TextBlock(finalPrompt)},
		})
		done <- promptDone{resp, perr}
	}()

	tr := newTranslator(cwd)
	relay := func(u sdk.SessionUpdate) bool {
		turns.observe(u)
		for _, spec := range tr.translate(u) {
			if !emit(spec) {
				return false
			}
		}
		return true
	}

	idleTimer := time.NewTimer(a.opts.IdleTimeout)
	defer idleTimer.Stop()
	resetIdle := func() {
		if !idleTimer.Stop() {
			select {
			case <-idleTimer.C:
			default:
			}
		}
		idleTimer.Reset(a.opts.IdleTimeout)
	}

	for {
		select {
		case u := <-h.updates:
			resetIdle()
			if !relay(u) {
				return nil
			}
		case d := <-done:
			for {
				select {
				case u := <-h.updates:
					if !relay(u) {
						return nil
					}
					continue
				default:
				}
				break
			}
			if d.err != nil {
				endPrompt(d.err)
				return fmt.Errorf("acp: prompt: %w%s", d.err, h.stderrTail())
			}
			// The Prompt RPC returns exactly once per round with its own
			// (not cumulative) usage - the round's usage is known here, once.
			// ctx wins per field, the shared stamp only fills blanks (#1048) -
			// same rule as traced.go's tracedModel and tools/emit.go's emitTool.
			recordUsage(a.opts.ModelName, ledger.FillBlankCoords(ledger.CoordsFromContext(ctx), coords), a.opts.Pricing, d.resp.Usage)
			if d.resp.StopReason == sdk.StopReasonRefusal {
				refusalErr := errors.New("acp: agent refused the prompt")
				endPrompt(refusalErr)
				return refusalErr
			}
			final := finalSpec(tr)
			a.log.Info("acp round done", "stop", string(d.resp.StopReason), "answer_len", len(final.parts[0].Text))
			promptSpan.SetAttributes(attribute.StringSlice(otelobs.GenAIResponseFinishReasons, []string{string(d.resp.StopReason)}))
			endPrompt(nil)
			emit(final)
			return nil
		case <-ctx.Done():
			a.gracefulCancel(h, sess.SessionId, done)
			return ctx.Err()
		case <-abortCtx.Done():
			a.gracefulCancel(h, sess.SessionId, done)
			return abortCtx.Err()
		case <-idleTimer.C:
			a.gracefulCancel(h, sess.SessionId, done)
			return fmt.Errorf("acp: no activity for %s - treating opencode as wedged%s", a.opts.IdleTimeout, h.stderrTail())
		}
	}
}

// mcpToolNames lists the exact MCP tool names this round offers, derived from
// the same MemSession the loopback server resolves per-request.
func mcpToolNames(sess vetting.MemSession, offered bool) []string {
	if !offered {
		return nil
	}
	var names []string
	add := func(tool string) { names = append(names, mcpServerName+"_"+tool) }
	add(toolCheckMermaid) // stateless, always offered
	if sess.Memory != nil {
		add(toolLoadMemory)
		add(toolStageMemory)
	}
	if sess.Review != nil {
		add(toolStageReviewComment)
		add(toolListReviewComments)
		add(toolUnstageReviewComment)
		add(toolStageReview)
	}
	if sess.PRStage != nil {
		if sess.ExistingPR {
			add(toolStagePush)
		} else {
			add(toolStagePR)
		}
	}
	return names
}

// mcpToolsBlock renders the offered MCP tool names as a round-start fact.
func mcpToolsBlock(names []string) string {
	if len(names) == 0 {
		return "MCP tools available to you this round: none."
	}
	return "MCP tools available to you this round:\n  " + strings.Join(names, ", ")
}

// gracefulCancel sends session/cancel and waits for the prompt goroutine to acknowledge.
func (a *Agent) gracefulCancel(h *procHandle, sessID sdk.SessionId, done <-chan promptDone) {
	cctx, cancel := context.WithTimeout(context.Background(), cancelGrace)
	defer cancel()
	_ = h.conn.Cancel(cctx, sdk.CancelNotification{SessionId: sessID})
	select {
	case <-done:
	case <-cctx.Done():
	}
}

// cancelGrace bounds how long a cancelled round waits for acknowledgement. A var so tests can shorten it.
var cancelGrace = 5 * time.Second

// newEvent wraps one translated spec as a session event with an explicit Branch.
func (a *Agent) newEvent(ctx adkagent.InvocationContext, spec eventSpec) *session.Event {
	ev := session.NewEvent(ctx, ctx.InvocationID())
	ev.Author = a.name
	ev.Branch = ctx.Branch()
	ev.Partial = spec.partial
	ev.Content = &genai.Content{Role: "model", Parts: spec.parts}
	if spec.usage != nil {
		ev.UsageMetadata = spec.usage
	}
	return ev
}

// contentText flattens a content's plain text parts.
func contentText(c *genai.Content) string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range c.Parts {
		if p != nil && p.Text != "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// inputText extracts the prompt from a node input.
func inputText(in any) string {
	switch v := in.(type) {
	case string:
		return v
	case *genai.Content:
		return contentText(v)
	default:
		return ""
	}
}
