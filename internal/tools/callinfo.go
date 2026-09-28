package tools

import (
	"context"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"

	"github.com/fagerbergj/quack/internal/vetting"
)

// CallScope is the DAG node an extension tool call reports as sdk.CallInfo.
// AdvisorToken is looked up directly: the prompt's last marker can be a foreign one.
type CallScope struct{ AdvisorToken, ChatID, UserID string }

// callInfoTool stamps sdk.CallInfo onto an extension tool's Run ctx. The gate
// only enforces staged deliveries: a tool that posts directly must honor it itself.
type callInfoTool struct {
	runnableTool
	scope CallScope
}

func withCallInfo(t tool.Tool, d Deps) tool.Tool {
	rt, ok := t.(runnableTool)
	if !ok {
		return t
	}
	return &callInfoTool{runnableTool: rt, scope: d.CallScope}
}

func (c *callInfoTool) ProcessRequest(ctx agent.Context, req *model.LLMRequest) error {
	return rebindToolMap(c.runnableTool, c, ctx, req)
}

func (c *callInfoTool) Run(ctx agent.Context, args any) (map[string]any, error) {
	return c.runnableTool.Run(valueCtx{Context: ctx, overlay: extsdk.WithCallInfo(context.Background(), c.callInfo(ctx))}, args)
}

func (c *callInfoTool) callInfo(ctx agent.Context) extsdk.CallInfo {
	if c.scope.AdvisorToken == "" { // not a DAG node: the run's own ctx carries its grant
		return extsdk.CallInfo{ChatID: ctx.SessionID(), UserID: ctx.UserID(),
			AllowedDeliveryKinds: sdkDeliveryKinds(AllowedDeliveryKindsFromContext(ctx)), ReadOnly: PlanOnlyFromContext(ctx)}
	}
	at, ok := vetting.LookupAdvisorThread(c.scope.AdvisorToken)
	if !ok { // a node whose grant is unknown fails closed
		return extsdk.CallInfo{ChatID: c.scope.ChatID, UserID: c.scope.UserID, AllowedDeliveryKinds: []extsdk.DeliveryKind{}, ReadOnly: true}
	}
	return extsdk.CallInfo{ChatID: at.ChatID, UserID: c.scope.UserID, AllowedDeliveryKinds: sdkDeliveryKinds(at.AllowedDeliveryKinds), ReadOnly: at.PlanOnly}
}

// sdkDeliveryKinds keeps nil (unrestricted) distinct from empty (deny-all).
func sdkDeliveryKinds(kinds []string) []extsdk.DeliveryKind {
	if kinds == nil {
		return nil
	}
	out := make([]extsdk.DeliveryKind, len(kinds))
	for i, k := range kinds {
		out[i] = extsdk.DeliveryKind(k)
	}
	return out
}

// valueCtx overlays CallInfo on an agent.Context, since ADK's tool ctx returns
// nil from WithAgentContext; the With* that work on a tool ctx keep the overlay.
type valueCtx struct {
	agent.Context
	overlay context.Context
}

func (c valueCtx) Value(key any) any {
	if v := c.overlay.Value(key); v != nil {
		return v
	}
	return c.Context.Value(key)
}

func (c valueCtx) WithAgentCancel() (agent.Context, context.CancelFunc) {
	inner, cancel := c.Context.WithAgentCancel()
	return valueCtx{Context: inner, overlay: c.overlay}, cancel
}

func (c valueCtx) WithDelta(d *agent.CommonContextDelta) agent.Context {
	return valueCtx{Context: c.Context.WithDelta(d), overlay: c.overlay}
}
