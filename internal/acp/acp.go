// Package acp runs an external coding agent (opencode, claude-agent-acp,
// gemini-cli, …) as a subprocess speaking the Agent Client Protocol — ndjson
// JSON-RPC 2.0 over stdio — and adapts it to an ADK agent quack's DAG executor
// and trust gate drive exactly like a native worker.
//
// One subprocess per worker round: spawn → initialize → session/new(cwd) →
// session/prompt → stream updates → kill. Revise/continuation prompts are
// self-contained by design (vetting.buildRevisionContent), so no state needs to
// survive between rounds; the repo on disk is the shared substrate.
// ponytail: process-per-round re-reads context each round — keep the process
// alive per node (keyed like nodeClient.ForNode) if round startup ever matters.
//
// The agent yields ADK session events translated from ACP session/update
// notifications, using QUACK's tool vocabulary (run_command, write_file,
// read_file — see mapToolCall) so the existing DAG stream, the trust-gate
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
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// Options configures one external ACP agent.
type Options struct {
	Command []string // argv to spawn, e.g. ["opencode", "acp"]
	// Env is EXTRA environment (KEY=VAL) appended after the minimal base
	// (PATH, HOME, OPENCODE_CONFIG_CONTENT, …) — later entries win, so an
	// operator override beats a generated default.
	Env []string
	// Home is the subprocess $HOME — the jail's isolated per-user home
	// (workspace.Caps.HomeDir), so the agent's own caches/state never land
	// inside a cloned repo.
	Home string
	// Preamble (the bundle's prompt.md) is prepended to every round's prompt —
	// the external agent controls its own system prompt, so this is the one
	// channel quack's per-agent guidance still reaches it through.
	Preamble string
	// Jail + UserID resolve the calling node's working directory from the
	// advisor-thread marker the gate stamps into every worker prompt.
	Jail   *workspace.Jail
	UserID string
	// StartTimeout bounds initialize + session/new (not the prompt itself,
	// which runs under the node's own context). 0 ⇒ 60s.
	StartTimeout time.Duration
	// PermissionJudge answers the agent's session/request_permission asks —
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

// resolveCwd derives the node's working directory from the advisor-thread
// marker in the prompt — the ONE channel that carries (chat, workspace-node)
// scope to a worker (the same one internal/tools uses). The setup clone lands
// AT the node root (workspace.SetupCloneDir == NodeDir), so this dir IS the
// repo for a setup-provisioned node.
func (a *Agent) resolveCwd(prompt string) (string, error) {
	token, ok := vetting.ParseAdvisorThread(prompt)
	if !ok {
		return "", errors.New("acp: prompt carries no workspace-scope marker (is this agent running outside the gate?)")
	}
	at, ok := vetting.LookupAdvisorThread(token)
	if !ok {
		return "", fmt.Errorf("acp: advisor thread %q not registered", token)
	}
	return a.opts.Jail.EnsureDir(a.opts.UserID, at.SessionID, workspace.NodeDir(at.WorkspaceNodeID))
}

// runPrompt is one full round: spawn, handshake, prompt, translate the update
// stream into session events, final answer event, shutdown.
func (a *Agent) runPrompt(ctx adkagent.InvocationContext, prompt string) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		if strings.TrimSpace(prompt) == "" {
			yield(nil, errors.New("acp: empty prompt"))
			return
		}
		cwd, err := a.resolveCwd(prompt)
		if err != nil {
			yield(nil, err)
			return
		}
		outbound := prompt
		if a.opts.Preamble != "" {
			outbound = a.opts.Preamble + "\n\n" + outbound
		}
		stopped := false
		err = a.round(ctx, cwd, outbound, func(spec eventSpec) bool {
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

// round drives one subprocess round: spawn, handshake, prompt, stream updates
// through the translator into emit, and emit the final answer spec last.
// Separated from runPrompt so it is drivable with a plain context in tests.
func (a *Agent) round(ctx context.Context, cwd, outbound string, emit func(eventSpec) bool) error {
	h, err := a.start(cwd)
	if err != nil {
		return err
	}
	defer h.close(a.log)

	ictx, cancelInit := context.WithTimeout(ctx, a.opts.StartTimeout)
	defer cancelInit()
	if _, err := h.conn.Initialize(ictx, sdk.InitializeRequest{
		ProtocolVersion: sdk.ProtocolVersionNumber,
		// No fs/terminal capabilities: the agent works directly on disk in
		// cwd; the jail scope + subprocess env are the boundary.
		ClientCapabilities: sdk.ClientCapabilities{},
	}); err != nil {
		return fmt.Errorf("acp: initialize: %w%s", err, h.stderrTail())
	}
	sess, err := h.conn.NewSession(ictx, sdk.NewSessionRequest{Cwd: cwd, McpServers: []sdk.McpServer{}})
	if err != nil {
		return fmt.Errorf("acp: session/new: %w%s", err, h.stderrTail())
	}
	a.log.Info("acp round started", "cwd", cwd, "session", sess.SessionId)

	type promptDone struct {
		resp sdk.PromptResponse
		err  error
	}
	done := make(chan promptDone, 1)
	go func() {
		// The RPC runs on its own context: on node cancel we want a graceful
		// session/cancel first, then the process kill — not an instant RPC abort.
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
	for {
		select {
		case u := <-h.updates:
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
				return fmt.Errorf("acp: prompt: %w%s", d.err, h.stderrTail())
			}
			if d.resp.StopReason == sdk.StopReasonRefusal {
				return errors.New("acp: agent refused the prompt")
			}
			// max_tokens / max_turn_requests: keep whatever answer streamed —
			// the gate's continuation/judge loop deals with incompleteness.
			final := finalSpec(tr)
			a.log.Info("acp round done", "stop", string(d.resp.StopReason), "answer_len", len(final.parts[0].Text))
			emit(final)
			return nil
		case <-ctx.Done():
			// Graceful cancel: session/cancel, a short grace for the agent to
			// resolve the prompt, then the deferred close kills the process group.
			cctx, cancel := context.WithTimeout(context.Background(), cancelGrace)
			_ = h.conn.Cancel(cctx, sdk.CancelNotification{SessionId: sess.SessionId})
			select {
			case <-done:
			case <-cctx.Done():
			}
			cancel()
			return ctx.Err()
		}
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
// for a media-capable node — ACP agents are text-only, media parts are dropped).
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
