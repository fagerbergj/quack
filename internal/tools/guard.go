// Package tools' guard ladder: a wrapper applied at tool-registration time
// around any tool named in workspace.guards, escalating a risky call through
// up to two tiers above the deterministic Tier-0 walls (the fs tools' path
// jail, and SSRF protection on agent-chosen URLs - unconditional, never
// bypassed by a guard result). Those walls confine WHERE a call can reach,
// not WHETHER it serves the task; the judge tier is what decides that:
//
//   - judge   - an independent safety-judge model call decides allow/deny;
//     a deny returns the refusal AS THE TOOL'S RESULT, never executing.
//   - confirm - the node pauses for human approve/deny, riding ask_user's
//     pause/resume mechanism (workflow.ResumeOrRequestInput).
//
// judge+confirm runs both: judge first (a denial short-circuits before
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
	"google.golang.org/adk/v2/tool/toolutils"

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
// ladder's Run. Passing g (not g.inner) is the whole point: registering the
// inner tool under the name would silently bypass every guard. PackTool became
// public in adk v2.1.0 (tool/toolutils); before that this body hand-replicated it.
func (g *guardedTool) ProcessRequest(_ agent.Context, req *model.LLMRequest) error {
	return toolutils.PackTool(req, g)
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
		// emits an adk_request_confirmation FunctionCall event that
		// scanNodeConfirms watches for to park the NODE. Returning
		// tool.ErrConfirmationRequired (functiontool's own sentinel) makes
		// this call's response the standard "requires confirmation" error,
		// and SkipSummarization ends the worker's turn so the gate sees an
		// empty draft + the pending confirmation, exactly like an ask_user
		// pause.
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
// lives - and asks vetting.ConfirmDecision whether the CURRENT call is the
// resolution of a just-answered confirm pause. mismatched reports a live
// decision pinned to DIFFERENT arguments (see Run).
//
// The session coordinates come from the guard-thread registry (guardSession),
// NEVER from this tool context's own AppName()/UserID()/SessionID(): over
// the A2A hop those name the A2A context session, a fresh per-round session
// holding none of the gate's events - scanning it would re-ask for
// confirmation on every approved re-issue. The gate registers the workflow
// coordinates per thread token at node entry (dag.newGatedNode →
// vetting.RegisterAdvisorThread), for co-located and A2A workers alike.
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
