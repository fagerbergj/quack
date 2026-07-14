package tools

import (
	"fmt"
	"log/slog"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/vetting"
)

// cancelGuard is the tool-layer half of node cancellation: a wrapper applied at
// tool-registration time (registry.go's Build) around EVERY built-in tool, which
// refuses the call outright when the calling node has been cancelled.
//
// Why the tool layer. vetting.NodeControl is cooperative and checked only at the
// gate's STAGE boundaries (before drafting, between judge rounds) — mid-model-call
// cancellation isn't available to us on ADK v2: the nodes of one plan share a
// runner and its event stream, so cancelling the ADK run's context out from under
// a worker would take out its siblings. That ceiling stands. But a worker in a
// 44-tool-call draft loop is many MINUTES from its next stage boundary, so a user
// cancel was indistinguishable from a no-op (live, 2026-07-13: "cancel and steer
// is seemingly doing nothing").
//
// Tools are the one thing such a worker does constantly, and they already know
// which node they run for — the advisor-thread marker in the prompt, the same
// identity channel the per-node jail and the guard ladder ride (see cwd.go's
// scopeFromContext). So the guard closes the gap where it is cheapest: a cancelled
// node's NEXT tool call fails immediately with an instruction the model cannot
// misread, and the gate's stage check remains the backstop that actually ends the
// node and keeps its partial answer (continue-but-warn).
//
// Latency after this: one tool call, not instant. That is the honest number.
type cancelGuard struct {
	inner     runnableTool
	cancelled func(chatID, nodeID string) bool
}

// cancelledMsg is what the model gets back instead of a tool result. It is an
// instruction, not a diagnostic: a model that reads "error" tends to retry, and a
// retry loop is exactly the behaviour cancel exists to stop.
const cancelledMsg = "This node was CANCELLED by the user. Stop calling tools. End your turn now with whatever you have."

// newCancelGuard wraps inner. inner must be a runnableTool (every tool this
// registry builds via functiontool.New is, and so is a guardedTool); anything
// else fails loudly at build time rather than silently shipping uncancellable.
func newCancelGuard(inner tool.Tool, cancelled func(chatID, nodeID string) bool) (tool.Tool, error) {
	rt, ok := inner.(runnableTool)
	if !ok {
		return nil, fmt.Errorf("tool %q does not support node cancellation (not a runnable function tool)", inner.Name())
	}
	return &cancelGuard{inner: rt, cancelled: cancelled}, nil
}

func (c *cancelGuard) Name() string        { return c.inner.Name() }
func (c *cancelGuard) Description() string { return c.inner.Description() }
func (c *cancelGuard) IsLongRunning() bool { return c.inner.IsLongRunning() }

func (c *cancelGuard) Declaration() *genai.FunctionDeclaration { return c.inner.Declaration() }

// ProcessRequest packs the WRAPPER into the request's tool map so the flow
// dispatches calls through this Run — delegating to inner.ProcessRequest would
// register the inner tool under the name and bypass the guard entirely. Same
// reason, same shape, as guardedTool.ProcessRequest (see guard.go).
func (c *cancelGuard) ProcessRequest(ctx agent.Context, req *model.LLMRequest) error {
	if err := c.inner.ProcessRequest(ctx, req); err != nil {
		return err
	}
	if req.Tools != nil {
		if _, ok := req.Tools[c.Name()]; ok {
			req.Tools[c.Name()] = c // re-point the dispatch entry at the wrapper
		}
	}
	return nil
}

// Run refuses the call when the calling node is cancelled, and is otherwise a
// straight pass-through — the hot path costs one map lookup under a mutex.
//
// A call with no node scope (no advisor-thread marker: a direct/un-gated
// invocation, an MCP call, the judge's own read tools) can't be attributed to a
// node and is never blocked.
func (c *cancelGuard) Run(ctx agent.Context, args any) (map[string]any, error) {
	if chatID, nodeID := nodeScope(ctx); nodeID != "" && c.cancelled(chatID, nodeID) {
		slog.Info("tool call refused: node cancelled by user", "component", "tools",
			"tool", c.Name(), "chat", chatID, "node", nodeID)
		return nil, fmt.Errorf("%s", cancelledMsg)
	}
	return c.inner.Run(ctx, args)
}

// nodeScope resolves the (chat, node) the call runs for, from the advisor-thread
// marker in the worker's prompt — the ONE identity channel that survives the A2A
// hop (a tool's own ctx.SessionID() names the A2A context session, not the chat).
// "", "" outside any gated node.
func nodeScope(ctx agent.Context) (chatID, nodeID string) {
	if ctx == nil {
		return "", ""
	}
	token, ok := vetting.ParseAdvisorThread(contentText(ctx.UserContent()))
	if !ok {
		return "", ""
	}
	at, ok := vetting.LookupAdvisorThread(token)
	if !ok {
		return "", ""
	}
	return at.SessionID, at.NodeID
}
