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

// cancelGuard: refuses calls when the calling node has been cancelled (latency: one tool call).
type cancelGuard struct {
	inner     runnableTool
	cancelled func(chatID, nodeID string) bool
}

// cancelledMsg: instruction to stop, not a diagnostic (retry loops defeat cancellation).
const cancelledMsg = "This node was CANCELLED by the user. Stop calling tools. End your turn now with whatever you have."

// newCancelGuard wraps inner; fails loudly if not runnable.
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

// ProcessRequest packs the wrapper into the request's tool map.
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

// Run: refuses if node cancelled; calls without node scope are never blocked.
func (c *cancelGuard) Run(ctx agent.Context, args any) (map[string]any, error) {
	if chatID, nodeID := nodeScope(ctx); nodeID != "" && c.cancelled(chatID, nodeID) {
		slog.Info("tool call refused: node cancelled by user", "component", "tools",
			"tool", c.Name(), "chat", chatID, "node", nodeID)
		return nil, fmt.Errorf("%s", cancelledMsg)
	}
	return c.inner.Run(ctx, args)
}

// nodeScope: resolves (chat, node) from the advisor-thread marker; ("", "") outside a gated node.
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
	return at.ChatID, at.NodeID
}
