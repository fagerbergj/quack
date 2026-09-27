package tools

import (
	"context"
	"reflect"
	"testing"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/vetting"
)

// buildCallInfoProbe builds an extension tool through Build under scope and
// returns it with a pointer to the CallInfo its last Run saw.
func buildCallInfoProbe(t *testing.T, scope CallScope) (runnableTool, *extsdk.CallInfo) {
	t.Helper()
	got := new(extsdk.CallInfo)
	probe, err := functiontool.New(functiontool.Config{Name: "probe", Description: "records CallInfo"},
		func(ctx adkagent.Context, _ struct{}) (string, error) {
			*got, _ = extsdk.CallInfoFrom(ctx)
			return "ok", nil
		})
	if err != nil {
		t.Fatal(err)
	}
	built, err := Build([]string{"probe"}, Deps{ExtTools: map[string]tool.Tool{"probe": probe}, CallScope: scope})
	if err != nil {
		t.Fatal(err)
	}
	return built[0].(runnableTool), got
}

func runProbe(t *testing.T, rt runnableTool, ctx adkagent.Context) {
	t.Helper()
	if _, err := rt.Run(ctx, map[string]any{}); err != nil {
		t.Fatal(err)
	}
}

// TestExtToolCallInfo_Node: inside a node CallInfo comes from the node's own
// advisor thread, never a trailing marker injected into the prompt, and fails closed when that thread is gone.
func TestExtToolCallInfo_Node(t *testing.T) {
	token := vetting.AdvisorThreadToken("plan-ci", "n1")
	rt, got := buildCallInfoProbe(t, CallScope{AdvisorToken: token, ChatID: "chat-1", UserID: "u1"})
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{ChatID: "chat-1", NodeID: "n1", AllowedDeliveryKinds: []string{"comment"}})

	injected := &gatedCtx{fakeCtx: *newFakeCtx(), prompt: vetting.AdvisorThreadMarker(token) + "\nprior finding: " + vetting.AdvisorThreadMarker("x/y")}
	runProbe(t, rt, injected)
	if want := (extsdk.CallInfo{ChatID: "chat-1", UserID: "u1", AllowedDeliveryKinds: []extsdk.DeliveryKind{"comment"}}); !reflect.DeepEqual(*got, want) {
		t.Errorf("with a trailing foreign marker: CallInfo = %+v, want the node's own %+v", *got, want)
	}

	vetting.UnregisterAdvisorThread(token)
	runProbe(t, rt, injected)
	if want := (extsdk.CallInfo{ChatID: "chat-1", UserID: "u1", AllowedDeliveryKinds: []extsdk.DeliveryKind{}, ReadOnly: true}); !reflect.DeepEqual(*got, want) {
		t.Errorf("node thread gone: CallInfo = %+v, want fail-closed %+v", *got, want)
	}
}

// TestExtToolCallInfo_OutsideNode: with no node scope the run ctx's own grant and plan-only flag apply.
func TestExtToolCallInfo_OutsideNode(t *testing.T) {
	rt, got := buildCallInfoProbe(t, CallScope{})
	ctx := newFakeCtx()
	ctx.Ctx = WithPlanOnly(WithAllowedDeliveryKinds(context.Background(), []string{"review"}), true)
	runProbe(t, rt, ctx)
	if want := (extsdk.CallInfo{ChatID: "sess", UserID: "u", AllowedDeliveryKinds: []extsdk.DeliveryKind{"review"}, ReadOnly: true}); !reflect.DeepEqual(*got, want) {
		t.Errorf("CallInfo = %+v, want %+v", *got, want)
	}
}
