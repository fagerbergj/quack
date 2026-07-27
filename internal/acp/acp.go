// Package acp runs an external coding agent (opencode, claude-agent-acp,
// gemini-cli, …) as a subprocess speaking the Agent Client Protocol - ndjson
// JSON-RPC 2.0 over stdio - and adapts it to an ADK agent quack's DAG executor
// and trust gate drive exactly like a native worker.
//
// One subprocess per worker round: spawn → initialize → session/new(cwd) →
// session/prompt → stream updates → kill. Revise/continuation prompts are
// self-contained by design (vetting.buildRevisionContent), so no state needs to
// survive between rounds; the repo on disk is the shared substrate.
// ponytail: process-per-round re-reads context each round - keep the process
// alive per node (keyed like nodeClient.ForNode) if round startup ever matters.
//
// The agent yields ADK session events translated from ACP session/update
// notifications, using QUACK's tool vocabulary (run_command, write_file,
// read_file - see mapToolCall) so the existing DAG stream, the trust-gate
// activity ledger (vetting.activityFromSessionAt) and the judge all keep
// working with no knowledge of ACP. Chunk deltas ride Partial events (streamed,
// never persisted); tool pairs and the final answer are durable events.
package acp

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"strings"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	"go.opentelemetry.io/otel/attribute"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// Options configures one external ACP agent.
type Options struct {
	Command []string // argv to spawn, e.g. ["opencode", "acp"]
	// Env is EXTRA environment (KEY=VAL) appended after the minimal base
	// (PATH, HOME, OPENCODE_CONFIG_CONTENT, …) - later entries win, so an
	// operator override beats a generated default. The caller (internal/serve)
	// pre-merges workspace.env under the agent's own acp.env before building
	// this, so agent-specific always wins here too.
	Env []string
	// Caps is the resolved workspace caps (internal/serve, once at startup) -
	// its Sandbox mode and ExtraPath (workspace.exec_path) drive both the
	// subprocess's hermetic PATH (workspace.ChildPath) and the OS boundary the
	// spawn is wrapped in (workspace.WrapArgv), exactly like every other
	// child process. ExtraRO adds grants on top (the skill paths handed to
	// opencode), which Caps has no notion of.
	Caps    workspace.Caps
	ExtraRO []string
	// Home is the subprocess $HOME - the jail's isolated per-user home
	// (workspace.Caps.HomeDir), so the agent's own caches/state never land
	// inside a cloned repo.
	Home string
	// Preamble (the bundle's prompt.md) is prepended to every round's prompt -
	// the external agent controls its own system prompt, so this is the one
	// channel quack's per-agent guidance still reaches it through.
	Preamble string
	// Jail + UserID resolve the calling node's working directory from the
	// advisor-thread marker the gate stamps into every worker prompt.
	Jail   *workspace.Jail
	UserID string
	// Worktree provisions a read-only qualifying node's (reviewer, explorer)
	// own git worktree, linked off the plan's shared setup clone - see
	// vetting.AdvisorTask.WorktreeParent. Called from resolveNode with
	// (userID, chatID, parentWorkspaceNodeID, thisWorkspaceNodeID); returns
	// the resolved absolute worktree dir. nil is fine as long as no plan ever
	// stamps a WorktreeParent (no code-review/explore agent configured, or no
	// plan.Setup) - a node that needs it with none configured is a wiring bug
	// (mirrors dag.SetupFunc's nil-executor error).
	Worktree func(ctx context.Context, userID, chatID, parentNodeID, nodeID string) (dir string, err error)
	// StartTimeout bounds initialize + session/new (not the prompt itself,
	// which runs under the node's own context). 0 ⇒ 60s.
	StartTimeout time.Duration
	// IdleTimeout bounds silence in a round: if opencode stops sending
	// session/updates AND the prompt RPC never returns (wedged), the round
	// would otherwise only unblock on the node's outer ctx (up to
	// defaultRunTimeout = 2h). Reset on every update, so a round that's slow
	// but alive - subprocess spin-up, model prefill before the first token -
	// is never killed. 0 ⇒ 10m.
	IdleTimeout time.Duration
	// PermissionJudge answers the agent's session/request_permission asks -
	// the ACP twin of the native guard ladder's judge tier. Everything a
	// round legitimately needs is already allowed in the generated config,
	// so an ask is the exceptional case (a directory escape, a .env read,
	// opencode's doom_loop detector); the judge decides it with context.
	// nil ⇒ allow (single-tenant deploys with the judge stage off trust the
	// container boundary, matching workspace.sandbox: none).
	PermissionJudge func(ctx context.Context, toolName, title string, input map[string]any) (allow bool, reason string)
}

// Agent is an adkagent.Agent backed by an external ACP subprocess. It
// implements the workflow node-runner interface (RunNode), so the gate hands it
// each round's prompt directly and no prompt session event is emitted
// (vetting.PromptEventNeeded).
type Agent struct {
	adkagent.Agent
	name string
	opts Options
	log  *slog.Logger
}

// New builds an ACP-backed agent. name/description feed the planner roster
// exactly like a bundle agent's.
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

// run is the plain-agent path (Run outside a workflow node): the prompt is the
// invocation's UserContent (AgentNode sets it from the node input too, so this
// also backstops any non-RunNode scheduling).
func (a *Agent) run(ic adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
	return a.runPrompt(ic, contentText(ic.UserContent()))
}

// RunNode is the node-runner path the gate's RunNode drives (vetting
// runWorkerNode): nodeInput is the fully-assembled round prompt.
func (a *Agent) RunNode(ctx adkagent.Context, nodeInput any) iter.Seq2[*session.Event, error] {
	return a.runPrompt(ctx, inputText(nodeInput))
}

// resolveNode derives the node's working directory AND its memory-MCP
// credential from the advisor-thread marker in the prompt - the ONE channel
// that carries (chat, workspace-node) scope to a worker (the same one
// internal/tools uses). The setup clone lands AT the node root
// (workspace.SetupCloneDir == NodeDir), so this dir IS the repo for a
// setup-provisioned node. A read-only qualifying node (at.WorktreeParent set
// - reviewer, explorer) gets its dir provisioned as a linked git worktree of
// the parent clone instead (Options.Worktree) - the worktree-per-node
// follow-up .quack/specs/sandbox-acp-landlock.md's "Out" section named.
//
// memSecret rides this SAME lookup (AdvisorTask.MemSecret) but is looked up
// in the SEPARATE memSessions registry when actually used (memoryMCPServers)
// - the advisor-thread token itself must never double as the memory MCP
// bearer credential; see vetting.AdvisorTask.MemSecret.
func (a *Agent) resolveNode(ctx context.Context, prompt string) (cwd, memSecret string, err error) {
	token, ok := vetting.ParseAdvisorThread(prompt)
	if !ok {
		return "", "", errors.New("acp: prompt carries no workspace-scope marker (is this agent running outside the gate?)")
	}
	at, ok := vetting.LookupAdvisorThread(token)
	if !ok {
		return "", "", fmt.Errorf("acp: advisor thread %q not registered", token)
	}
	if at.WorktreeParent != "" {
		if a.opts.Worktree == nil {
			return "", "", fmt.Errorf("acp: node %q needs a git worktree but no worktree executor is configured", at.NodeID)
		}
		cwd, err = a.opts.Worktree(ctx, a.opts.UserID, at.SessionID, at.WorktreeParent, at.WorkspaceNodeID)
		return cwd, at.MemSecret, err
	}
	cwd, err = a.opts.Jail.EnsureDir(a.opts.UserID, at.SessionID, workspace.NodeDir(at.WorkspaceNodeID))
	return cwd, at.MemSecret, err
}

// runPrompt is one full round: spawn, handshake, prompt, translate the update
// stream into session events, final answer event, shutdown.
func (a *Agent) runPrompt(ctx adkagent.InvocationContext, prompt string) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		if strings.TrimSpace(prompt) == "" {
			yield(nil, errors.New("acp: empty prompt"))
			return
		}
		cwd, memSecret, err := a.resolveNode(ctx, prompt)
		if err != nil {
			yield(nil, err)
			return
		}
		// The environment block grounds the round in what's ACTUALLY on disk
		// (the environment-grounding follow-up) - observation, not
		// instruction, so it never has to compete with a task's own prose the
		// way the deleted "do not clone the repo" clauses did.
		outbound := environmentBlock(ctx, cwd, a.opts.Caps) + "\n\n" + prompt
		if a.opts.Preamble != "" {
			outbound = a.opts.Preamble + "\n\n" + outbound
		}
		stopped := false
		err = a.round(ctx, cwd, memSecret, outbound, func(spec eventSpec) bool {
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

// promptDone carries the Prompt RPC's outcome off the goroutine in round.
type promptDone struct {
	resp sdk.PromptResponse
	err  error
}

// round drives one subprocess round: spawn, handshake, prompt, stream updates
// through the translator into emit, and emit the final answer spec last.
// Separated from runPrompt so it is drivable with a plain context in tests.
// memSecret is this node's memory-MCP credential ("" disables the surface for
// the round); it never rides outbound - see resolveNode.
func (a *Agent) round(ctx context.Context, cwd, memSecret, outbound string, emit func(eventSpec) bool) (err error) {
	ctx, roundSpan := otelobs.Start(ctx, "acp.round", attribute.String("agent", a.name), attribute.String("cwd", cwd))
	defer func() { otelobs.End(roundSpan, err) }()

	spawnCtx, spawnSpan := otelobs.Start(ctx, "acp.spawn", attribute.String("agent", a.name))
	_ = spawnCtx
	h, err := a.start(cwd)
	otelobs.End(spawnSpan, err)
	if err != nil {
		return err
	}
	defer h.close(a.log)

	ictx, cancelInit := context.WithTimeout(ctx, a.opts.StartTimeout)
	defer cancelInit()
	handshakeCtx, handshakeSpan := otelobs.Start(ctx, "acp.handshake", attribute.String("agent", a.name))
	_ = handshakeCtx
	initResp, err := h.conn.Initialize(ictx, sdk.InitializeRequest{
		ProtocolVersion: sdk.ProtocolVersionNumber,
		// No fs/terminal capabilities: the agent works directly on disk in
		// cwd; the jail scope + subprocess env are the boundary.
		ClientCapabilities: sdk.ClientCapabilities{},
	})
	if err != nil {
		otelobs.End(handshakeSpan, err)
		return fmt.Errorf("acp: initialize: %w%s", err, h.stderrTail())
	}
	// The memory MCP surface (#344) is keyed by memSecret - an unguessable
	// per-node credential resolved in-process (resolveNode), NEVER placed in
	// outbound: an untrusted external subprocess can already see its running
	// siblings' node IDs in its own prompt, and the advisor-thread token those
	// IDs would let it reconstruct must never double as a bearer credential.
	mcpServers := memoryMCPServers(memSecret, initResp.AgentCapabilities)
	// #482: the negotiated caps decide whether load_memory/stage_memory/
	// stage_review reach this subprocess at all - log them so a "tools
	// unavailable" report is diagnosable from prod logs instead of guessed at.
	a.log.Debug("acp negotiated capabilities", "mcp_http", initResp.AgentCapabilities.McpCapabilities.Http,
		"mcp_sse", initResp.AgentCapabilities.McpCapabilities.Sse, "mcp_acp", initResp.AgentCapabilities.McpCapabilities.Acp,
		"mcp_surface_offered", len(mcpServers) > 0, "has_mem_secret", memSecret != "")
	sess, err := h.conn.NewSession(ictx, sdk.NewSessionRequest{Cwd: cwd, McpServers: mcpServers})
	if err != nil {
		otelobs.End(handshakeSpan, err)
		return fmt.Errorf("acp: session/new: %w%s", err, h.stderrTail())
	}
	handshakeSpan.SetAttributes(attribute.String("session_id", string(sess.SessionId)))
	otelobs.End(handshakeSpan, nil)
	a.log.Info("acp round started", "cwd", cwd, "session", sess.SessionId)

	done := make(chan promptDone, 1)
	_, promptSpan := otelobs.Start(ctx, "acp.prompt", attribute.String("agent", a.name), attribute.String("session_id", string(sess.SessionId)))
	defer promptSpan.End() // safety net for the relay-stopped/cancel exits below; the done-branch sets the real status first
	go func() {
		// The RPC runs on its own context: on node cancel we want a graceful
		// session/cancel first, then the process kill - not an instant RPC abort.
		// Deliberately NOT ctx (would inherit its cancellation) - the span is
		// still opened above, against ctx, so it nests correctly regardless.
		resp, perr := h.conn.Prompt(context.Background(), sdk.PromptRequest{
			SessionId: sess.SessionId,
			Prompt:    []sdk.ContentBlock{sdk.TextBlock(outbound)},
		})
		done <- promptDone{resp, perr}
	}()

	tr := newTranslator(cwd)
	relay := func(u sdk.SessionUpdate) bool {
		for _, spec := range tr.translate(u) {
			if !emit(spec) {
				return false
			}
		}
		return true
	}

	// idleTimer fires when the round has gone silent: no update AND no
	// prompt-done for IdleTimeout. Reset on every update so a round that's
	// slow-but-alive (spin-up, model prefill) is never killed for it.
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
				otelobs.End(promptSpan, d.err)
				return fmt.Errorf("acp: prompt: %w%s", d.err, h.stderrTail())
			}
			if d.resp.StopReason == sdk.StopReasonRefusal {
				refusalErr := errors.New("acp: agent refused the prompt")
				otelobs.End(promptSpan, refusalErr)
				return refusalErr
			}
			// max_tokens / max_turn_requests: keep whatever answer streamed -
			// the gate's continuation/judge loop deals with incompleteness.
			final := finalSpec(tr)
			a.log.Info("acp round done", "stop", string(d.resp.StopReason), "answer_len", len(final.parts[0].Text))
			promptSpan.SetAttributes(attribute.String("stop_reason", string(d.resp.StopReason)))
			otelobs.End(promptSpan, nil)
			emit(final)
			return nil
		case <-ctx.Done():
			a.gracefulCancel(h, sess.SessionId, done)
			return ctx.Err()
		case <-idleTimer.C:
			a.gracefulCancel(h, sess.SessionId, done)
			return fmt.Errorf("acp: no activity for %s - treating opencode as wedged%s", a.opts.IdleTimeout, h.stderrTail())
		}
	}
}

// gracefulCancel sends session/cancel and waits up to cancelGrace for the
// prompt goroutine to acknowledge before returning; the caller's deferred
// h.close then kills the process group. Shared by the ctx.Done() and
// idle-timeout exits so the two paths can't drift.
func (a *Agent) gracefulCancel(h *procHandle, sessID sdk.SessionId, done <-chan promptDone) {
	cctx, cancel := context.WithTimeout(context.Background(), cancelGrace)
	defer cancel()
	_ = h.conn.Cancel(cctx, sdk.CancelNotification{SessionId: sessID})
	select {
	case <-done:
	case <-cctx.Done():
	}
}

// cancelGrace is how long a cancelled round waits for the agent to acknowledge
// session/cancel before the process group is killed outright. A var so a test
// can shorten it.
var cancelGrace = 5 * time.Second

// newEvent wraps one translated spec as a session event. Branch is stamped
// explicitly: a branchless event is visible to EVERY branch of the shared plan
// session (agent.eventBelongsToBranch), so a sibling A2A worker's outbound
// message would otherwise textify this node's tool activity into its request.
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

// inputText extracts the prompt from a node input (string, or *genai.Content
// for a media-capable node - ACP agents are text-only, media parts are dropped).
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
