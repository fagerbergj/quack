package sdk_test

import (
	"context"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack-extensions/sdk"
)

// invocation is the minimum InvocationContext agent.NewToolContext touches.
type invocation struct{ agent.StrictContextMock }

func (*invocation) Artifacts() agent.Artifacts { return nil }

// TestCallInfoReachesFunctionTool runs a real functiontool through ADK's own
// tool context, so a host that attaches CallInfo to the run ctx is what a tool sees.
func TestCallInfoReachesFunctionTool(t *testing.T) {
	want := sdk.CallInfo{ChatID: "ext:github:acme-widgets-7", UserID: "u1", AllowedDeliveryKinds: []sdk.DeliveryKind{}, ReadOnly: true}
	var got sdk.CallInfo
	var gotOK bool
	tl, err := functiontool.New(functiontool.Config{Name: "probe", Description: "probe"},
		func(ctx agent.Context, _ struct{}) (struct{}, error) {
			got, gotOK = sdk.CallInfoFrom(ctx)
			return struct{}{}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	run := tl.(interface {
		Run(agent.Context, any) (map[string]any, error)
	}).Run

	ic := &invocation{agent.NewStrictContextMock(sdk.WithCallInfo(context.Background(), want))}
	if _, err := run(agent.NewToolContext(ic, "call-1", nil, nil), map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if !gotOK || got.ChatID != want.ChatID || got.UserID != "u1" || !got.ReadOnly || got.AllowedDeliveryKinds == nil || len(got.AllowedDeliveryKinds) != 0 {
		t.Errorf("CallInfoFrom = %+v, %v; want %+v (deny-all kept non-nil)", got, gotOK, want)
	}

	bare := &invocation{agent.NewStrictContextMock(context.Background())}
	if _, err := run(agent.NewToolContext(bare, "call-2", nil, nil), map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if gotOK {
		t.Errorf("CallInfoFrom without WithCallInfo: ok = true, want false (older host)")
	}
}
