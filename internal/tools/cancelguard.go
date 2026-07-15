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

// cancelGuard is the tool-layer half of node cancellation: applied around EVERY
// built-in tool at registration (registry.Build), it refuses the call when the
// calling node has been cancelled. The gate's stage check is cooperative and can
// be many minutes of tool loop away (mid-model-call cancellation would take out
// sibling nodes sharing the runner — that ceiling stands), so the guard makes a
// cancel land on the node's NEXT tool call. Latency: one tool call, not instant.
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
