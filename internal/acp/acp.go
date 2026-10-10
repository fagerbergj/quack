// Package acp runs an external coding agent as an ACP subprocess behind an ADK agent: one subprocess
// per node, pinned across that node's rounds and torn down when the node finishes, fails reuse, or is cancelled.
package acp

import (
	"context"
	"encoding/json"
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

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// Options configures one external ACP agent.
type Options struct {
	Command []string // argv to spawn, e.g. the pi-acp shim: ["node", "/usr/local/lib/pi-acp/pi-acp.mjs"]
	Env     []string
	Caps    workspace.Caps
	// SkillPaths is read at every spawn, for both the sandbox's ExtraRO grant and PI_ACP_CONFIG's skill_paths,
	// since a pinned process only re-spawns between nodes.
	SkillPaths func() []string
	// ExtraRO grants sandbox RO access only, never skill_paths, so plugins.root stays readable
	// without pi's recursive skill scan seeing every SKILL.md in the registry.
	ExtraRO func() []string
	// Plugins, if set, is read per round for the agent.invoke ledger entry's plugin provenance.
	Plugins func() []ledger.PluginRef
	Home    string
	// Preamble is rebuilt each round that sends one; round.go prepends it only on a fresh session,
	// so on a pinned process an edited prompt lands on the next node dispatch, not the next round.
	Preamble func(ctx context.Context) string
	// PreambleArtifact reads back the exact artifact the last Preamble build used -
	// called under the same !fromPinned condition, never on a reused session.
	PreambleArtifact func(ctx context.Context) artifactsrc.Artifact
	// MemoryArtifact is the bundle's memory.md, folded by Preamble into the system prompt text;
	// static for this agent's lifetime.
	MemoryArtifact artifactsrc.Artifact
	// Prompts resolves system/acp.environment for the round's environment block.
	Prompts         *artifactsrc.Resolver
	Jail            *workspace.Jail
	UserID          string
	Worktree        func(ctx context.Context, userID, chatID, parentNodeID, nodeID string) (dir string, err error)
	StartTimeout    time.Duration
	IdleTimeout     time.Duration
	PermissionJudge func(ctx context.Context, toolName, title string, input map[string]any) (allow bool, reason string)
	// ModelName: the model this agent's PI_ACP_CONFIG binds it to -
	// attrs the round's gen_ai metrics.
	ModelName string
	// Pricing: nil = no price table entry for ModelName, cost metric skipped.
	Pricing *config.ModelPricing
	// RegisterLiveSteer/UnregisterLiveSteer let a queued message land mid-round instead of at the
	// next gate boundary. nil = park always.
	RegisterLiveSteer   func(chatID, nodeID string, forward func(text string) bool)
	UnregisterLiveSteer func(chatID, nodeID string)
	// RegisterRoundAbort/UnregisterRoundAbort let CancelNode reach a running round's abort RPC directly.
	// Cancel only, never pause: pause must keep what the round accumulated so it can resume.
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

	// newIdleTimer is a clock seam so tests drive the idle-wedge timer deterministically.
	newIdleTimer func(time.Duration) idleTimer
}

// idleTimer is the subset of *time.Timer that round's idle watchdog needs.
type idleTimer interface {
	C() <-chan time.Time
	Stop() bool
	Reset(time.Duration) bool
}

type realIdleTimer struct{ *time.Timer }

func (t realIdleTimer) C() <-chan time.Time { return t.Timer.C }

// SetLedgerCoords stamps coordinates for the next round. round() copies them at start rather than
// reading live: this Agent is shared across concurrent nodes and a round can run for minutes.
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
	a := &Agent{name: name, opts: opts, log: slog.With("component", "acp", "agent", name),
		newIdleTimer: func(d time.Duration) idleTimer { return realIdleTimer{time.NewTimer(d)} }}
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

// resolveNode derives the node's cwd, memory-MCP credential and scratch dirs from the token dag stamps
// on the round ctx, never the prompt; no token or no registration fails the round before any spawn.
func (a *Agent) resolveNode(ctx context.Context) (cwd, memSecret, scratchDir, acpStateDir string, readOnly bool, chatID, nodeID, token, priorSessionID string, err error) {
	token = vetting.AdvisorTokenFromContext(ctx)
	if token == "" {
		return "", "", "", "", false, "", "", "", "", errors.New("acp: round ctx carries no node token (is this agent running outside the gate?)")
	}
	at, ok := vetting.LookupAdvisorThread(token)
	if !ok {
		return "", "", "", "", false, "", "", "", "", fmt.Errorf("acp: advisor thread %q not registered", token)
	}
	chatID, nodeID, priorSessionID = at.ChatID, at.NodeID, at.ACPSessionID
	if a.opts.Jail != nil {
		// A read-only reviewer needs scratch as much as a writer (TMPDIR/mktemp/heredocs);
		// scoped per node so concurrent rounds never collide.
		scratchDir, err = a.opts.Jail.ScratchDir(a.opts.UserID, at.ChatID, at.WorkspaceNodeID)
		if err != nil {
			return "", "", "", "", false, chatID, nodeID, token, priorSessionID, fmt.Errorf("acp: scratch dir: %w", err)
		}
		acpStateDir, err = a.opts.Jail.ACPStateDir(a.opts.UserID, at.ChatID, at.WorkspaceNodeID)
		if err != nil {
			return "", "", "", "", false, chatID, nodeID, token, priorSessionID, fmt.Errorf("acp: state dir: %w", err)
		}
	}
	if at.WorktreeParent != "" {
		if a.opts.Worktree == nil {
			return "", "", "", "", false, chatID, nodeID, token, priorSessionID, fmt.Errorf("acp: node %q needs a git worktree but no worktree executor is configured", at.NodeID)
		}
		cwd, err = a.opts.Worktree(ctx, a.opts.UserID, at.ChatID, at.WorktreeParent, at.WorkspaceNodeID)
		return cwd, at.MemSecret, scratchDir, acpStateDir, at.ReadOnly, chatID, nodeID, token, priorSessionID, err
	}
	cwd, err = a.opts.Jail.EnsureDir(a.opts.UserID, at.ChatID, workspace.NodeDir(at.WorkspaceNodeID))
	return cwd, at.MemSecret, scratchDir, acpStateDir, at.ReadOnly, chatID, nodeID, token, priorSessionID, err
}

// runPrompt is one full round: spawn, handshake, prompt, stream translation, shutdown.
func (a *Agent) runPrompt(ctx adkagent.InvocationContext, prompt string) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		if strings.TrimSpace(prompt) == "" {
			yield(nil, errors.New("acp: empty prompt"))
			return
		}
		cwd, memSecret, scratchDir, acpStateDir, readOnly, steerChatID, steerNodeID, advisorToken, priorSessionID, err := a.resolveNode(ctx)
		if err != nil {
			yield(nil, err)
			return
		}
		// homeTmpDir recreates scratch at the next spawn; removing it here keeps build tmp files
		// from lingering until the gc TTL sweep.
		if scratchDir != "" {
			defer func() { _ = os.RemoveAll(scratchDir) }()
		}
		// caps.ReadOnly comes from this node's advisor task, not the agent's static config:
		// a planOnly run forces it true per node.
		caps := a.opts.Caps
		caps.ReadOnly = readOnly
		caps.ScratchDir = scratchDir
		caps.ACPStateDir = acpStateDir
		// The environment block goes after the task: it changes every round (branch/HEAD/listing),
		// so leading with it would break the prompt-cache prefix from round 2 on.
		envBlock, envArt := environmentBlock(ctx, a.opts.Prompts, cwd, caps)
		outbound := prompt + "\n\n" + envBlock
		stopped := false
		err = a.round(ctx, cwd, memSecret, caps, outbound, envArt, steerChatID, steerNodeID, advisorToken, priorSessionID, func(spec eventSpec) bool {
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

// steerExtMethod: ACP extension forwarding a mid-round steer to the shim.
const steerExtMethod = "_quack/steer"

type steerParams struct {
	Text string `json:"text"`
}

// extensionCaller is the one *sdk.ClientSideConnection method steerForward needs,
// narrowed so forwarding is unit-testable without a real ACP subprocess.
type extensionCaller interface {
	CallExtension(ctx context.Context, method string, params any) (json.RawMessage, error)
}

// steerForward builds the RegisterLiveSteer callback: an acked CallExtension RPC, not a fire-and-forget notify.
func steerForward(conn extensionCaller) func(text string) bool {
	return func(text string) bool {
		_, err := conn.CallExtension(context.Background(), steerExtMethod, steerParams{Text: text})
		return err == nil
	}
}

// promptDone carries the Prompt RPC's outcome off the goroutine in round.
type promptDone struct {
	resp sdk.PromptResponse
	err  error
}

// pinnedProc is one node's live ACP subprocess, kept across its rounds: the shim keeps pi alive for its
// stdio session, so a second session/prompt on the same connection carries history with no replay.
type pinnedProc struct {
	h         *procHandle
	sessID    sdk.SessionId
	toolNames []string
}

// pinned: advisorToken -> the node's pinned process, shared across Agents and concurrent nodes
// (the token is unique per node instance, vetting.AdvisorThreadToken).
var pinned sync.Map

// ClosePinnedSession kills token's pinned process (wired to vetting.NodeSessionClosed; acp can't import vetting).
// The shim's on-disk session stays for later reuse until chat archive/delete (workspace.Jail.RemoveACPState).
func ClosePinnedSession(token string) {
	if v, ok := pinned.LoadAndDelete(token); ok {
		closePinnedProc(v.(*pinnedProc))
	}
}

// CloseAllPinnedSessions kills every pinned process at shutdown, covering a force-cancelled
// round whose node-finish hook never fired.
func CloseAllPinnedSessions() {
	pinned.Range(func(k, v any) bool {
		closePinnedProc(v.(*pinnedProc))
		pinned.Delete(k)
		return true
	})
}

func closePinnedProc(pp *pinnedProc) {
	pp.h.close(nil)
}

// roundArtifacts: this round's resolved-artifact provenance - the environment block always,
// the preamble (with memory.md) only when sentPreamble.
func (a *Agent) roundArtifacts(ctx context.Context, envArt artifactsrc.Artifact, sentPreamble bool) []ledger.ArtifactRef {
	var artifacts []ledger.ArtifactRef
	if envArt.Name != "" {
		artifacts = append(artifacts, ledger.ArtifactRef{Name: envArt.Name, Source: envArt.Source, VersionID: envArt.VersionID})
	}
	if sentPreamble {
		if a.opts.PreambleArtifact != nil {
			if art := a.opts.PreambleArtifact(ctx); art.Name != "" {
				artifacts = append(artifacts, ledger.ArtifactRef{Name: art.Name, Source: art.Source, VersionID: art.VersionID})
			}
		}
		if mem := a.opts.MemoryArtifact; mem.Name != "" {
			artifacts = append(artifacts, ledger.ArtifactRef{Name: mem.Name, Source: mem.Source, VersionID: mem.VersionID})
		}
	}
	return artifacts
}

// round drives one subprocess round. caps is the node's effective caps (ReadOnly resolved by the caller).
// steerChatID/steerNodeID are the advisor thread's ids, not ledger.Coords, whose NodeID can collapse to a shared scope.
func (a *Agent) round(ctx context.Context, cwd, memSecret string, caps workspace.Caps, outbound string, envArt artifactsrc.Artifact, steerChatID, steerNodeID, advisorToken, priorSessionID string, emit func(eventSpec) bool) (err error) {
	ctx, roundSpan := otelobs.Start(ctx, "acp.round", attribute.String(otelobs.GenAIAgentName, a.name), attribute.String("cwd", cwd))
	defer func() { otelobs.End(roundSpan, err) }()
	// This round's own chat/node (the advisor thread's), ahead of the agent-wide stamp below:
	// concurrent nodes share one Agent, so the stamp can belong to another chat.
	ctx = ledger.WithCoords(ctx, ledger.FillBlankCoords(ledger.CoordsFromContext(ctx), ledger.Coords{ChatID: steerChatID, Node: steerNodeID}))

	// Snapshot now, before any subprocess I/O - see SetLedgerCoords.
	a.mu.Lock()
	coords := a.coords
	a.mu.Unlock()

	// abortCtx is CancelNode's direct line into this round, separate from ctx (which also carries
	// shutdown) so both reach the same graceful-cancel path without masking each other's cause.
	abortCtx, abortCancel := context.WithCancel(context.Background())
	defer abortCancel()
	unregRoundAbort := a.registerRoundAbort(steerChatID, steerNodeID, abortCancel)
	defer unregRoundAbort()

	// Reuse this node's live pinned process (the common case from round 2 on): skips spawn,
	// Initialize and session/new|load, since history already lives in the process.
	var h *procHandle
	var sessID sdk.SessionId
	var toolNames []string
	fromPinned := false
	resumed := false
	if advisorToken != "" {
		if v, ok := pinned.Load(advisorToken); ok {
			pp := v.(*pinnedProc)
			h, sessID, toolNames = pp.h, pp.sessID, pp.toolNames
			fromPinned = true
		}
	}

	if !fromPinned {
		spawnCtx, spawnSpan := otelobs.Start(ctx, "acp.spawn", attribute.String(otelobs.GenAIAgentName, a.name))
		_ = spawnCtx
		h, err = a.startLive(ctx, cwd, caps)
		otelobs.End(spawnSpan, err)
		if err != nil {
			return err
		}
	}

	// pinOK, not err==nil, gates reuse: a round the caller stopped consuming returns nil err without a
	// real PromptResponse, and its process must not carry a prompt in flight into the next round.
	var pinOK bool
	defer func() {
		if pinOK && advisorToken != "" {
			pinned.Store(advisorToken, &pinnedProc{h: h, sessID: sessID, toolNames: toolNames})
			return
		}
		if advisorToken != "" {
			pinned.Delete(advisorToken)
		}
		h.close(a.log)
	}()
	// Each round gets its own slice of the teed wire, so a pinned process doesn't carry prior rounds'
	// bytes into this round's invoke_agent event or toward maxTeeBytes.
	h.sent.reset()
	h.received.reset()
	var plugins []ledger.PluginRef
	if a.opts.Plugins != nil {
		plugins = a.opts.Plugins()
	}
	// artifacts is filled in below, AFTER steerHooks - Preamble's build (called
	// from steerHooks) is what stashes PreambleArtifact's value for this round.
	var artifacts []ledger.ArtifactRef
	defer func() { emitInvokeAgent(ctx, a.name, h.sent, h.received, err, plugins, artifacts) }()

	if !fromPinned {
		sessID, toolNames, resumed, err = a.handshake(ctx, cwd, memSecret, advisorToken, priorSessionID, h)
		if err != nil {
			return err
		}
		if advisorToken != "" && resumed {
			// A resumed session that then errors out is probably dead server-side -
			// don't hand the next round a session id that will just fail LoadSession again.
			defer func() {
				if err != nil {
					vetting.SetAdvisorThreadSessionID(advisorToken, "")
				}
			}()
		}
	} else {
		a.log.Info("acp round reusing pinned session", "cwd", cwd, "session", sessID)
	}

	outbound, sentPreamble, unregSteer := a.steerHooks(ctx, h, outbound, steerChatID, steerNodeID, fromPinned)
	defer unregSteer()
	artifacts = a.roundArtifacts(ctx, envArt, sentPreamble)

	finalPrompt := mcpToolsBlock(toolNames) + "\n\n" + outbound

	al, promptCleanup, perr := a.prepPrompt(ctx, abortCtx, h, cwd, sessID, finalPrompt, coords, emit)
	if perr != nil {
		return perr
	}
	defer promptCleanup()
	pinOK, err = a.roundLoop(al)
	return err
}

// prepPrompt: the pre-prompt cancel bail, the Prompt RPC goroutine, and span/timer plumbing.
// Returns the loop args plus round-exit cleanup (LIFO), or the bail error when a cancel lands first.
func (a *Agent) prepPrompt(ctx, abortCtx context.Context, h *procHandle, cwd string, sessID sdk.SessionId, finalPrompt string, coords ledger.Coords, emit func(eventSpec) bool) (*roundLoopArgs, func(), error) {
	done := make(chan promptDone, 1)
	promptCtx, promptSpan := otelobs.Start(ctx, "acp.prompt", attribute.String(otelobs.GenAIAgentName, a.name), attribute.String("session_id", string(sessID)))
	// Per-tool-call child spans, ended as updates arrive: the only telemetry that reaches a collector mid-round.
	turns := newTurnSpans(promptCtx, a.name)
	endPrompt := func(err error) {
		turns.closeAll()
		otelobs.End(promptSpan, err)
	}
	// A cancel during spawn/handshake has nothing to cancel yet: session/cancel for an unsent prompt is a
	// no-op and waiting on done blocks the full cancelGrace. Bail before sending session/prompt.
	select {
	case <-ctx.Done():
		endPrompt(ctx.Err())
		return nil, nil, ctx.Err()
	case <-abortCtx.Done():
		endPrompt(abortCtx.Err())
		return nil, nil, abortCtx.Err()
	default:
	}
	go func() {
		resp, perr := h.conn.Prompt(context.Background(), sdk.PromptRequest{
			SessionId: sessID,
			Prompt:    []sdk.ContentBlock{sdk.TextBlock(finalPrompt)},
		})
		done <- promptDone{resp, perr}
	}()
	idleTimer := a.newIdleTimer(a.opts.IdleTimeout)
	cleanup := func() {
		idleTimer.Stop()
		turns.closeAll()
		promptSpan.End() // safety net for the relay-stopped/cancel exits; the done-branch sets the real status first
	}
	al := &roundLoopArgs{h: h, sessID: sessID, done: done, tr: newTranslator(cwd), turns: turns, endPrompt: endPrompt, promptSpan: promptSpan, idleTimer: idleTimer, ctx: ctx, abortCtx: abortCtx, coords: coords, emit: emit}
	return al, cleanup, nil
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
		add(toolRecallMemory)
	}
	if sess.Artifacts != nil {
		add(toolReadArtifact)
		add(toolListArtifacts)
		add(toolEditArtifact)
		add(toolWriteArtifact)
		for _, spec := range recordstore.Kinds() {
			if !spec.AgentWritable {
				continue // mirrors registerArtifactWriteTools' own skip: gate-only kind
			}
			// Mirrors registerArtifactWriteTools' own skip.
			if spec.Name() == "code_review" && sess.Review != nil && sess.Review.IsNonDeliveringSlice() {
				continue
			}
			add(writeKindPrefix + spec.Name())
		}
	}
	if sess.Review != nil {
		add(toolStageReviewComment)
		add(toolListReviewComments)
		add(toolUnstageReviewComment)
		if !sess.Review.IsNonDeliveringSlice() {
			add(toolStageReview)
		}
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

// gracefulCancel sends session/cancel and waits for the prompt goroutine to ack. Cancel's write can wedge on
// writeMu if the child stopped draining stdin, so it runs on its own goroutine to make cancelGrace a real bound.
func (a *Agent) gracefulCancel(h *procHandle, sessID sdk.SessionId, done <-chan promptDone) {
	cctx, cancel := context.WithTimeout(context.Background(), cancelGrace)
	defer cancel()
	errc := make(chan error, 1)
	go func() { errc <- h.conn.Cancel(cctx, sdk.CancelNotification{SessionId: sessID}) }()
	select {
	case err := <-errc:
		if err != nil {
			return // broken pipe etc - nothing more to wait for
		}
		select {
		case <-done:
		case <-cctx.Done():
		}
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
