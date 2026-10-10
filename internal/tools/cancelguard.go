package tools

import (
	"fmt"
	"log/slog"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"

	"github.com/fagerbergj/quack/internal/ledger"
)

// cancelGuard: refuses calls when the calling node has been cancelled (latency: one tool call).
type cancelGuard struct {
	runnableTool
	cancelled func(chatID, nodeID string) bool
	scope     CallScope
}

// cancelledMsg: instruction to stop, not a diagnostic (retry loops defeat cancellation).
const cancelledMsg = "This node was CANCELLED by the user. Stop calling tools. End your turn now with whatever you have."

func newCancelGuard(inner tool.Tool, cancelled func(chatID, nodeID string) bool, scope CallScope) (tool.Tool, error) {
	rt, ok := inner.(runnableTool)
	if !ok {
		return nil, fmt.Errorf("tool %q does not support node cancellation (not a runnable function tool)", inner.Name())
	}
	return &cancelGuard{runnableTool: rt, cancelled: cancelled, scope: scope}, nil
}

func (c *cancelGuard) SetLedgerCoords(coords ledger.Coords) {
	if cs, ok := c.runnableTool.(ledger.CoordSetter); ok {
		cs.SetLedgerCoords(coords)
	}
}

func (c *cancelGuard) ProcessRequest(ctx agent.Context, req *model.LLMRequest) error {
	return rebindToolMap(c.runnableTool, c, ctx, req)
}

// Run: refuses if node cancelled; calls without node scope, or whose thread is gone, are never blocked.
func (c *cancelGuard) Run(ctx agent.Context, args any) (map[string]any, error) {
	if chatID, nodeID := c.scope.node(ctx); nodeID != "" && c.cancelled(chatID, nodeID) {
		slog.Info("tool call refused: node cancelled by user", "component", "tools",
			"tool", c.Name(), "chat", chatID, "node", nodeID)
		return nil, fmt.Errorf("%s", cancelledMsg)
	}
	return c.runnableTool.Run(ctx, args)
}

// node is the calling node's (chat, node); ("", "") outside a node or when its thread is not registered.
func (s CallScope) node(ctx agent.Context) (chatID, nodeID string) {
	at, _ := s.advisorTask(ctx)
	return at.ChatID, at.NodeID
}
