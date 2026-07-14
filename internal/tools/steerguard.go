package tools

import (
	"fmt"
	"log/slog"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

// steerGuard is the tool-layer half of node steering: a wrapper applied at
// tool-registration time (registry.go's Build) around EVERY built-in tool,
// sibling to cancelGuard. Where cancel refuses the call, steer INTERCEPTS it:
// the calling node's next tool call, if it has undelivered steer guidance
// pending, does not run the real tool at all — it gets the guidance back as
// that call's result instead, worded as an instruction to adjust course now.
//
// Why the tool layer, same reasoning as cancelGuard: vetting.NodeControl's
// TakeSteer is cooperative and only checked at the gate's stage boundaries
// (before drafting, between judge rounds — internal/vetting/node.go), never
// mid-draft. A worker deep in a tool-calling draft loop can be minutes from its
// next boundary, so a steer sent while it's drafting was silently swallowed
// until the WHOLE draft finished — live, 2026-07-14: steer "takes too long to
// take effect" was really "doesn't land until the draft ends". The gate-stage
// check remains: it still consumes the guidance (TakeSteer) and drives a full,
// clean re-run of the node with the guidance folded into the prompt — that
// stays the thorough backstop. This guard is the immediate nudge in between.
type steerGuard struct {
	inner    runnableTool
	guidance func(chatID, nodeID string) string // "" ⇒ nothing pending; consumes what it returns
}

// newSteerGuard wraps inner. inner must be a runnableTool, same requirement as
// newCancelGuard.
func newSteerGuard(inner tool.Tool, guidance func(chatID, nodeID string) string) (tool.Tool, error) {
	rt, ok := inner.(runnableTool)
	if !ok {
		return nil, fmt.Errorf("tool %q does not support node steering (not a runnable function tool)", inner.Name())
	}
	return &steerGuard{inner: rt, guidance: guidance}, nil
}

func (s *steerGuard) Name() string        { return s.inner.Name() }
func (s *steerGuard) Description() string { return s.inner.Description() }
func (s *steerGuard) IsLongRunning() bool { return s.inner.IsLongRunning() }

func (s *steerGuard) Declaration() *genai.FunctionDeclaration { return s.inner.Declaration() }

// ProcessRequest re-points the dispatch entry at the wrapper — same reasoning
// as cancelGuard.ProcessRequest.
func (s *steerGuard) ProcessRequest(ctx agent.Context, req *model.LLMRequest) error {
	if err := s.inner.ProcessRequest(ctx, req); err != nil {
		return err
	}
	if req.Tools != nil {
		if _, ok := req.Tools[s.Name()]; ok {
			req.Tools[s.Name()] = s
		}
	}
	return nil
}

// Run delivers pending steer guidance as the call's RESULT (not an error —
// unlike cancel, this call isn't refused, it's redirected) and skips the real
// tool this once. A call with no node scope is never intercepted, same as
// cancelGuard.
func (s *steerGuard) Run(ctx agent.Context, args any) (map[string]any, error) {
	if chatID, nodeID := nodeScope(ctx); nodeID != "" {
		if g := s.guidance(chatID, nodeID); g != "" {
			slog.Info("tool call intercepted: steer guidance delivered", "component", "tools",
				"tool", s.Name(), "chat", chatID, "node", nodeID)
			return map[string]any{
				"result": fmt.Sprintf("STEER: the user redirected you mid-task: %q. Adjust your approach now — then retry any tool call you still need.", g),
			}, nil
		}
	}
	return s.inner.Run(ctx, args)
}
