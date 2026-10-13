package extproc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/fagerbergj/quack/extensions/sdk"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
)

type echoArgs struct {
	Msg string `json:"msg"`
}

type echoResult struct {
	Msg      string `json:"msg"`
	Chat     string `json:"chat,omitempty"`
	CallID   string `json:"call_id"`
	ReadOnly bool   `json:"read_only,omitempty"`
}

// echoTool is long-running so its declaration's description differs from Description().
func echoTool() tool.Tool {
	t, _ := functiontool.New(functiontool.Config{Name: "echo", Description: "Echo msg.", IsLongRunning: true},
		func(ctx agent.Context, a echoArgs) (echoResult, error) {
			switch a.Msg {
			case "fail":
				return echoResult{}, errors.New("echo refused")
			case "wait":
				fmt.Fprintln(os.Stderr, "tool waiting")
				<-ctx.Done()
				fmt.Fprintln(os.Stderr, "tool cancelled")
				return echoResult{}, ctx.Err()
			}
			ci, _ := sdk.CallInfoFrom(ctx)
			return echoResult{Msg: a.Msg, Chat: ci.ChatID, CallID: ctx.FunctionCallID(), ReadOnly: ci.ReadOnly}, nil
		})
	return t
}

var (
	testSchemas = map[string]json.RawMessage{"note": json.RawMessage(`{"type":"object","required":["z","a"]}`)}
	testPoints  = []sdk.DecisionPoint{{Name: "keep", Description: "keep it?", Primary: "keep", Modes: []string{"observe"},
		Questions: map[string]sdk.DecisionQuestion{"keep": {Type: "score", Instructions: "how much?", Criteria: []string{"low", "high"}}}}}
)

// decideState's fields are out of alphabetical order, so a map in transit would reorder them.
type decideState struct {
	Zeta  int    `json:"zeta"`
	Alpha string `json:"alpha"`
}

func toolCtx(ctx context.Context, callID string) agent.Context {
	return agent.NewToolContext(&toolInvocation{agent.NewStrictContextMock(ctx)}, callID, nil, nil)
}

func remoteEcho(t *testing.T, p *Process) functionTool {
	t.Helper()
	if len(p.Tools()) != 1 {
		t.Fatalf("Tools() = %d tools, want the child's one", len(p.Tools()))
	}
	return p.Tools()[0].(functionTool)
}

// The proxy must look and answer exactly as the child's own tool does in-process.
func TestToolCrossesTheWire(t *testing.T) {
	p, _, _ := startProcess(t)
	local, remote := echoTool().(functionTool), remoteEcho(t, p)
	if remote.Name() != "echo" || remote.Description() != local.Description() || !remote.IsLongRunning() {
		t.Errorf("remote tool = %q %q %t, want echo %q true", remote.Name(), remote.Description(), remote.IsLongRunning(), local.Description())
	}
	want, _ := json.Marshal(local.Declaration())
	if got, _ := json.Marshal(remote.Declaration()); string(got) != string(want) {
		t.Errorf("declaration\n got %s\nwant %s", got, want)
	}

	ci := sdk.CallInfo{ChatID: "ext:x:1", UserID: "u", AllowedDeliveryKinds: []sdk.DeliveryKind{}, ReadOnly: true}
	for _, ctx := range []context.Context{sdk.WithCallInfo(context.Background(), ci), context.Background()} {
		for _, msg := range []string{"hi", "fail"} {
			args := map[string]any{"msg": msg}
			want, wantErr := local.Run(toolCtx(ctx, "call-1"), args)
			got, gotErr := remote.Run(toolCtx(ctx, "call-1"), args)
			if !reflect.DeepEqual(got, want) || fmt.Sprint(gotErr) != fmt.Sprint(wantErr) {
				t.Errorf("Run(%q) = %v, %v; in process %v, %v", msg, got, gotErr, want, wantErr)
			}
		}
	}

	req := &model.LLMRequest{}
	if err := remote.(*remoteTool).ProcessRequest(nil, req); err != nil || req.Tools["echo"] != remote ||
		req.Config.Tools[0].FunctionDeclarations[0] != remote.Declaration() {
		t.Errorf("ProcessRequest packed %+v, %v; want the proxy under echo", req, err)
	}
}

func TestToolCallFailures(t *testing.T) {
	p, _, _ := startProcess(t)
	ctx := toolCtx(context.Background(), "c")
	if _, err := (&remoteTool{p: p, spec: toolSpec{Name: "nope"}}).Run(ctx, map[string]any{}); err == nil || !strings.Contains(err.Error(), `unknown tool "nope"`) {
		t.Errorf("unknown tool = %v", err)
	}
	if _, err := remoteEcho(t, p).Run(ctx, "msg"); err == nil || !strings.Contains(err.Error(), "unexpected args type") {
		t.Errorf("non-map args = %v", err)
	}
	_ = p.Close()
	if _, err := remoteEcho(t, p).Run(ctx, map[string]any{"msg": "hi"}); !errors.Is(err, errClosed) {
		t.Errorf("tool call after Close = %v, want errClosed", err)
	}
}

func TestToolCallCancelReachesChild(t *testing.T) {
	p, _, logs := startProcess(t)
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, err := remoteEcho(t, p).Run(toolCtx(ctx, "c"), map[string]any{"msg": "wait"})
		errc <- err
	}()
	waitFor(t, "the tool to run in the child", func() bool { return strings.Contains(logs.String(), `"line":"tool waiting"`) })
	cancel()
	waitFor(t, "the child tool's ctx to end", func() bool { return strings.Contains(logs.String(), `"line":"tool cancelled"`) })
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled tool call = %v", err)
	}
}

func TestHostCallbacksCrossTheWire(t *testing.T) {
	var mu sync.Mutex
	var states []string
	decision := sdk.Decision{Top: "true", TopP: 0.9, Outcome: "observe"}
	p, srv, _ := startProcessHost(t, sdk.Host{
		ReadArtifact: func(chatID, user, name string) ([]byte, bool) {
			if chatID != "ext:x:1" || user != "u" || name != "a" {
				return nil, false
			}
			return []byte("A"), true
		},
		Decide: func(_ context.Context, req sdk.DecideRequest) (sdk.Decision, error) {
			if req.Point == "bad" {
				return sdk.Decision{}, errors.New("no such point")
			}
			b, _ := json.Marshal(req.State)
			mu.Lock()
			defer mu.Unlock()
			states = append(states, req.Point+" "+req.Baseline+" "+string(b))
			return decision, nil
		},
	})
	for path, want := range map[string]string{
		"/x/callbacks":        "read=true decide=true",
		"/x/read?name=a":      "A true",
		"/x/read?name=b":      " false",
		"/x/decide?point=p":   fmt.Sprintf("%+v <nil>", decision),
		"/x/decide?point=bad": fmt.Sprintf("%+v no such point", sdk.Decision{}),
	} {
		if _, body := get(t, srv.URL+path); body != want {
			t.Errorf("GET %s = %q, want %q", path, body, want)
		}
	}
	inProcess, _ := json.Marshal(decideState{Zeta: 1, Alpha: "<a>"})
	mu.Lock()
	if want := []string{"p true " + string(inProcess)}; !reflect.DeepEqual(states, want) {
		t.Errorf("Decide saw %q, want %q", states, want)
	}
	mu.Unlock()

	gotSchemas, _ := json.Marshal(p.ArtifactSchemas())
	wantSchemas, _ := json.Marshal(testSchemas)
	gotPoints, _ := json.Marshal(p.DecisionPoints())
	wantPoints, _ := json.Marshal(testPoints)
	if string(gotSchemas) != string(wantSchemas) || string(gotPoints) != string(wantPoints) {
		t.Errorf("schemas %s points %s; want %s %s", gotSchemas, gotPoints, wantSchemas, wantPoints)
	}
}

// A callback quack does not serve stays nil in the child, as it would in-process.
func TestCallbacksNilWhenHostLacksThem(t *testing.T) {
	_, srv, _ := startProcess(t)
	if _, body := get(t, srv.URL+"/x/callbacks"); body != "read=false decide=false" {
		t.Errorf("callbacks = %q", body)
	}
}

// A host that predates tool calls would drop the child's tools, schemas and points without a word.
func TestChildRefusesAHostWithoutTools(t *testing.T) {
	s := &server{factory: testFactory, log: slog.New(slog.DiscardHandler)}
	raw, _ := json.Marshal(initParams{Protocol: ProtocolVersion, Name: "x"})
	if _, err := s.initialize(raw); err == nil || !strings.Contains(err.Error(), "takes no tools") {
		t.Fatalf("initialize from a host without tool = %v", err)
	}
}

type plainTool struct{}

func (plainTool) Name() string        { return "plain" }
func (plainTool) Description() string { return "" }
func (plainTool) IsLongRunning() bool { return false }

func TestDescribeToolsRefusesANonFunctionTool(t *testing.T) {
	if _, _, err := describeTools([]tool.Tool{plainTool{}}); err == nil || !strings.Contains(err.Error(), `"plain"`) {
		t.Fatalf("describeTools = %v", err)
	}
}
