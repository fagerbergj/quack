// Package tools' guard ladder (see .quack/plan-pr5-tool-schemas.md §4b): a
// wrapper applied at tool-registration time (registry.go's Build) around any
// tool named in workspace.guards, escalating a risky call through up to two
// tiers above the deterministic Tier-0 walls (the fs/git tools' path jail, and
// the OS sandbox + rlimits around their child processes - unconditional, never
// bypassed by a guard result). "No shell" is NOT one of those walls: a
// sandboxed run_command takes a real shell command line (workspace.RunShell),
// and the judge tier is what decides what that command may do:
//
//   - judge   - an independent safety-judge model call decides allow/deny.
//     Deny returns the refusal AS THE TOOL'S RESULT; the tool never executes.
//   - confirm - the node pauses for a human approve/deny, riding the exact
//     node-level pause/resume mechanism ask_user already proves out
//     (workflow.ResumeOrRequestInput; see internal/vetting/confirm.go).
//
// judge+confirm runs both: judge first (a denial short-circuits before ever
// asking a human), confirm second.
package tools

import (
	"fmt"
	"log/slog"
	"strings"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"

	"github.com/fagerbergj/quack/internal/vetting"
)

// runnableTool is the structural interface a functiontool-built tool.Tool
// actually satisfies beyond the plain tool.Tool interface (Name/Description/
// IsLongRunning) - Declaration (the LLM-facing schema) and Run (the actual
// invocation). guardedTool needs all of it to stand in for inner
// transparently. Mirrors ADK's own (unexported) tool.runnableTool.
type runnableTool interface {
	tool.Tool
	Declaration() *genai.FunctionDeclaration
	Run(ctx agent.Context, args any) (map[string]any, error)
	ProcessRequest(ctx agent.Context, req *model.LLMRequest) error
}

// guardTier is a parsed workspace.guards value.
type guardTier struct {
	Judge   bool
	Confirm bool
}

// parseGuardTier parses a workspace.guards entry. ok is false for "" or
// "none" (unguarded). config.Load validates guard values at startup (see
// internal/config), so an unrecognized string here defensively falls back to
// unguarded rather than silently over-restricting.
func parseGuardTier(s string) (guardTier, bool) {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "judge":
		return guardTier{Judge: true}, true
	case "confirm":
		return guardTier{Confirm: true}, true
	case "judge+confirm":
		return guardTier{Judge: true, Confirm: true}, true
	default:
		return guardTier{}, false
	}
}

// guardedTool wraps a runnableTool with the guard ladder. Constructed once per
// agent build (registry.go's Build), shared across every invocation of that
// agent - all bookkeeping is derived fresh from session history per call (see
// vetting.ConfirmDecision), so sharing is safe across concurrent nodes/runs.
type guardedTool struct {
	inner    runnableTool
	tier     guardTier
	judge    SafetyJudge
	sessions session.Service
}

// newGuardedTool wraps inner with tier. inner must be a runnableTool (every
// tool this registry builds via functiontool.New is); a tool that isn't
// (a future non-function tool kind) fails loudly at build time rather than
// silently shipping unguarded.
func newGuardedTool(inner tool.Tool, tier guardTier, judge SafetyJudge, sessions session.Service) (tool.Tool, error) {
	rt, ok := inner.(runnableTool)
	if !ok {
		return nil, fmt.Errorf("tool %q does not support the guard ladder (not a runnable function tool)", inner.Name())
	}
	return &guardedTool{inner: rt, tier: tier, judge: judge, sessions: sessions}, nil
}

func (g *guardedTool) Name() string        { return g.inner.Name() }
func (g *guardedTool) Description() string { return g.inner.Description() }
func (g *guardedTool) IsLongRunning() bool { return g.inner.IsLongRunning() }

func (g *guardedTool) Declaration() *genai.FunctionDeclaration { return g.inner.Declaration() }

// ProcessRequest packs the WRAPPER - not the inner tool - into the request's
// tool map, so the flow dispatches this tool's calls through the guard
// ladder's Run. Delegating to inner.ProcessRequest would register the INNER
// tool under the name and silently bypass every guard. Replicates ADK's
// toolutils.PackTool (internal, unimportable) - exactly what ADK's own
// confirmationTool wrapper does.
func (g *guardedTool) ProcessRequest(_ agent.Context, req *model.LLMRequest) error {
	if req.Tools == nil {
		req.Tools = make(map[string]any)
	}
	name := g.Name()
	if _, ok := req.Tools[name]; ok {
		return fmt.Errorf("duplicate tool: %q", name)
	}
	req.Tools[name] = g
	if req.Config == nil {
		req.Config = &genai.GenerateContentConfig{}
	}
	decl := g.Declaration()
	if decl == nil {
		return nil
	}
	for _, tl := range req.Config.Tools {
		if tl != nil && tl.FunctionDeclarations != nil {
			tl.FunctionDeclarations = append(tl.FunctionDeclarations, decl)
			return nil
		}
	}
	req.Config.Tools = append(req.Config.Tools, &genai.Tool{FunctionDeclarations: []*genai.FunctionDeclaration{decl}})
	return nil
}

// Run is the guard ladder itself. See the package doc for the tier order.
func (g *guardedTool) Run(ctx agent.Context, args any) (map[string]any, error) {
	m, _ := args.(map[string]any)

	// mismatch: a resolved-but-unconsumed decision exists for this tool with
	// DIFFERENT arguments than this call's. The decision is pinned to the
	// exact operation the human approved (vetting.ConfirmDecision), so this
	// call falls through as a brand-new proposal - full judge tier, fresh
	// confirmation - and the hint below tells the human it differs.
	var mismatch bool
	if g.tier.Confirm {
		approved, matched, mm := g.confirmDecision(ctx, m)
		mismatch = mm
		if matched {
			if !approved {
				return markResolved(guardRefusal("denied by user confirmation")), nil
			}
			result, err := g.inner.Run(ctx, args)
			if err != nil {
				return nil, err
			}
			return markResolved(result), nil
		}
	}

	if g.tier.Judge {
		allow, reason, err := g.runSafetyJudge(ctx, m)
		if err != nil {
			// Fail CLOSED: an unavailable safety judge must never silently grant a
			// guarded operation.
			slog.Warn("safety judge unavailable; failing closed", "component", "tools", "tool", g.Name(), "err", err)
			return guardRefusal("safety check unavailable: " + err.Error()), nil
		}
		if !allow {
			return guardRefusal(reason), nil
		}
	}

	if g.tier.Confirm {
		// ADK-NATIVE confirmation request: RequestConfirmation records the
		// pending confirmation on this call's EventActions, and the llm flow
		// then emits an `adk_request_confirmation` FunctionCall event
		// (toolconfirmation.FunctionCallName) carrying the original call -
		// which is what the trust gate's scanNodeConfirms watches for to park
		// the NODE (internal/vetting/confirm.go). Returning
		// tool.ErrConfirmationRequired (the same sentinel functiontool's own
		// RequireConfirmation path uses) makes this call's own response the
		// standard "requires confirmation" error, and SkipSummarization ends
		// the worker's turn so the gate sees an empty draft + the pending
		// confirmation, exactly like an ask_user pause.
		hint := fmt.Sprintf("Approve running %s? Reply \"approve\" or \"deny\".", g.Name())
		if mismatch {
			hint = fmt.Sprintf("Approve running %s? NOTE: this operation DIFFERS from the one you previously "+
				"approved for this tool - review its arguments carefully. Reply \"approve\" or \"deny\".", g.Name())
		}
		if err := ctx.RequestConfirmation(hint, nil); err != nil {
			return nil, err
		}
		ctx.Actions().SkipSummarization = true
		return nil, fmt.Errorf("tool %q %w", g.Name(), tool.ErrConfirmationRequired)
	}

	result, err := g.inner.Run(ctx, args)
	if err != nil {
		return nil, err
	}
	return result, nil
}

// confirmDecision fetches the WORKFLOW session - where every confirm event
// lives (the adk_request_confirmation calls, the human's resume
// FunctionResponse, and the GuardResolvedKey consumption markers) - and asks
// vetting.ConfirmDecision whether the CURRENT call (this exact tool name +
// arguments) is the resolution of a just-answered confirm pause. mismatched
// reports a live decision pinned to DIFFERENT arguments (see Run).
//
// The session coordinates come from the guard-thread registry (guardSession),
// NEVER from this tool context's own AppName()/UserID()/SessionID(): over the
// A2A hop the tool executes inside the A2A server's runner, where those name
// the A2A context session - a fresh per-round session that holds none of the
// gate's events. Scanning it found nothing, so every approved re-issue looked
// like a brand-new proposal and re-asked for confirmation forever (a live
// failure). The gate registers the workflow coordinates per thread token at
// node entry (dag.newGatedNode → vetting.RegisterAdvisorThread), for
// co-located and A2A workers alike - one lookup path.
func (g *guardedTool) confirmDecision(ctx agent.Context, args map[string]any) (approved, matched, mismatched bool) {
	if g.sessions == nil {
		return false, false, false
	}
	sess, invocationID, nodeID := g.guardSession(ctx)
	if sess == nil {
		return false, false, false
	}
	return vetting.ConfirmDecision(sess, invocationID, nodeID, g.Name(), args)
}

// guardSession resolves the calling node's WORKFLOW session + invocation +
// node ID: the thread token from the prompt marker (guardThread) keys the
// registry entry the gate wrote at node entry, which carries the workflow
// session's coordinates. nil session when this call runs outside any gated
// node (no marker / no registration) - the guard then has no confirm history
// and no task context, which fails toward MORE restriction, never less.
func (g *guardedTool) guardSession(ctx agent.Context) (sess session.Session, invocationID, nodeID string) {
	token, nodeID := guardThread(ctx)
	if nodeID == "" {
		return nil, "", ""
	}
	at, ok := vetting.LookupAdvisorThread(token)
	if !ok || at.SessionID == "" {
		return nil, "", ""
	}
	resp, err := g.sessions.Get(ctx, &session.GetRequest{AppName: at.AppName, UserID: at.UserID, SessionID: at.SessionID})
	if err != nil || resp == nil || resp.Session == nil {
		return nil, "", ""
	}
	return resp.Session, at.InvocationID, nodeID
}

// runSafetyJudge invokes the judge tier, best-effort-deriving the node's task
// (session state, seeded by dag.newGatedNode - mirrors ask_advisor.go's
// seedText) and a short recent-activity summary for context. A nil g.judge
// (SafetyJudge unconfigured - see registry.go's buildAgents wiring) fails
// closed rather than skipping the check.
func (g *guardedTool) runSafetyJudge(ctx agent.Context, args map[string]any) (allow bool, reason string, err error) {
	if g.judge == nil {
		return false, "", fmt.Errorf("no safety judge configured")
	}
	var task, activity string
	if g.sessions != nil {
		if token, nodeID := guardThread(ctx); nodeID != "" {
			if at, found := vetting.LookupAdvisorThread(token); found {
				task = at.Task
			}
			// The activity summary must come from the WORKFLOW session too - the
			// A2A context session this tool may be running under holds only the
			// current dispatch (see confirmDecision).
			if sess, invocationID, _ := g.guardSession(ctx); sess != nil {
				activity = recentActivity(sess, invocationID)
			}
		}
	}
	return g.judge(ctx, "", task, g.Name(), args, activity)
}

// guardThread resolves the gated node this call runs under, from the same
// prompt marker ask_advisor uses (vetting.AdvisorThreadMarker - the ONE
// identity channel that survives the A2A hop; see advisor_thread.go). token is
// the full plan/node thread token (keys the registered task for the safety
// judge); nodeID is its node segment (keys the confirm scan). Both empty for a
// direct, un-gated invocation - the guard then runs with no task context and
// no confirm history, which fails toward MORE restriction, never less.
func guardThread(tc agent.Context) (token, nodeID string) {
	tok, ok := vetting.ParseAdvisorThread(contentText(tc.UserContent()))
	if !ok {
		return "", ""
	}
	if i := strings.LastIndex(tok, "/"); i >= 0 && i+1 < len(tok) {
		return tok, tok[i+1:]
	}
	return tok, ""
}

// recentActivity summarizes the last few tool calls this invocation made in
// the WORKFLOW session (see guardSession), for the safety judge's context.
func recentActivity(sess session.Session, invocationID string) string {
	var calls []string
	for ev := range sess.Events().All() {
		if ev == nil || ev.Content == nil || ev.InvocationID != invocationID {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.FunctionCall != nil {
				calls = append(calls, p.FunctionCall.Name)
			}
		}
	}
	const maxRecent = 8
	if len(calls) > maxRecent {
		calls = calls[len(calls)-maxRecent:]
	}
	if len(calls) == 0 {
		return ""
	}
	return "  - " + strings.Join(calls, "\n  - ")
}

// guardRefusal / markResolved build the guard ladder's FunctionResponse
// results - see vetting/confirm.go for the marker vocabulary the trust gate
// watches for.
func guardRefusal(reason string) map[string]any {
	return map[string]any{vetting.GuardStatusKey: "denied", "reason": reason}
}

func markResolved(result map[string]any) map[string]any {
	if result == nil {
		result = map[string]any{}
	}
	result[vetting.GuardResolvedKey] = true
	return result
}
