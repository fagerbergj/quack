// Guard ladder: judge (model deny) + confirm (human approve/deny). judge+confirm = judge first, short-circuits.
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

// runnableTool: tool.Tool extended with Declaration/Run/ProcessRequest.
type runnableTool interface {
	tool.Tool
	Declaration() *genai.FunctionDeclaration
	Run(ctx agent.Context, args any) (map[string]any, error)
	ProcessRequest(ctx agent.Context, req *model.LLMRequest) error
}

// guardTier: parsed workspace.guards value.
type guardTier struct {
	Judge   bool
	Confirm bool
}

// parseGuardTier: parses guard entry. "" or "none" = unguarded.
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

// guardedTool: wraps a runnableTool with the guard ladder. Shared across invocations (bookkeeping derived fresh).
type guardedTool struct {
	inner    runnableTool
	tier     guardTier
	judge    SafetyJudge
	sessions session.Service
}

// newGuardedTool wraps inner with tier; fails loudly if not runnable.
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

// ProcessRequest: packs the wrapper, not the inner tool, into the request's tool map.
func (g *guardedTool) ProcessRequest(_ agent.Context, req *model.LLMRequest) error {
	return toolutils.PackTool(req, g)
}

// Run is the guard ladder itself. See the package doc for the tier order.
func (g *guardedTool) Run(ctx agent.Context, args any) (map[string]any, error) {
	m, _ := args.(map[string]any)

	// mismatch: resolved decision exists for this tool but with different arguments.
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
		// Fail closed: unavailable safety judge must never silently grant.
			slog.Warn("safety judge unavailable; failing closed", "component", "tools", "tool", g.Name(), "err", err)
			return guardRefusal("safety check unavailable: " + err.Error()), nil
		}
		if !allow {
			return guardRefusal(reason), nil
		}
	}

	if g.tier.Confirm {
		// ADK-native confirmation request. Ends the worker's turn until answered.
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

// confirmDecision: checks if the current call resolves a just-answered confirm pause. Uses guard-thread registry, not tool context.
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

// guardSession: resolves the calling node's workflow session from the guard-thread registry.
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

// runSafetyJudge: invokes judge tier. nil judge fails closed.
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
			// Activity from workflow session, not A2A context session.
			if sess, invocationID, _ := g.guardSession(ctx); sess != nil {
				activity = recentActivity(sess, invocationID)
			}
		}
	}
	return g.judge(ctx, "", task, g.Name(), args, activity)
}

// guardThread: resolves the gated node from the prompt marker; ("", "") for un-gated invocations.
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

// recentActivity: summarizes recent tool calls for the safety judge.
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

// guardRefusal / markResolved: build guard ladder's FunctionResponse results.
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
