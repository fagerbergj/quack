package mcp

import (
	"context"
	"iter"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/stream"
)

// fakeOrch stands in for the orchestrator: a session's first run pauses with a pending question;
// once answered is set, run completes with a token derived from the message.
type fakeOrch struct {
	question string
	answered bool
}

func (f *fakeOrch) run(_ context.Context, _, message string) iter.Seq2[stream.SSEEvent, error] {
	return func(yield func(stream.SSEEvent, error) bool) {
		if !f.answered {
			return
		}
		yield(stream.SSEEvent{Data: stream.AgentTokenData{Text: "resolved: " + message}}, nil)
	}
}

func (f *fakeOrch) pending(_ context.Context, _ string) (orchestrator.PendingQuestion, bool) {
	if f.answered {
		return orchestrator.PendingQuestion{}, false
	}
	f.answered = true
	return orchestrator.PendingQuestion{Message: f.question}, true
}

// connect wires the ask tool onto an in-memory MCP server/client pair backed by f,
// like the go-sdk's mrtr_test.go mustConnect.
func connect(t *testing.T, f *fakeOrch, clientOpts *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "quack-test", Version: "0.0.0"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "ask"}, newAskHandler(f.run, f.pending))

	st, ct := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.0"}, clientOpts)
	cs, err := client.Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func textOf(res *mcp.CallToolResult) string {
	if len(res.Content) == 0 {
		return ""
	}
	tc, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		return ""
	}
	return tc.Text
}

// TestAskHandler_ManualRoundTrip drives MRTR by hand (client middleware disabled): the first call
// surfaces input_required with an echoable RequestState, and the retry carries the elicitation answer.
func TestAskHandler_ManualRoundTrip(t *testing.T) {
	tests := []struct {
		name        string
		elicit      *mcp.ElicitResult
		wantContent string
	}{
		{
			name:        "accept",
			elicit:      &mcp.ElicitResult{Action: "accept", Content: map[string]any{"answer": "blue"}},
			wantContent: "resolved: blue",
		},
		{
			name:        "decline",
			elicit:      &mcp.ElicitResult{Action: "decline"},
			wantContent: "cancelled: no answer provided",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeOrch{question: "which color?"}
			cs := connect(t, f, &mcp.ClientOptions{
				MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true},
			})

			res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
				Name:      "ask",
				Arguments: AskInput{Query: "what should I paint?", SessionID: "s1"},
			})
			if err != nil {
				t.Fatalf("CallTool() error = %v", err)
			}
			if !res.NeedsInput() {
				t.Fatalf("NeedsInput() = false on first call, want true")
			}
			ir, ok := res.InputRequests[mrtrAnswerID].(*mcp.ElicitParams)
			if !ok {
				t.Fatalf("InputRequests[%q] type = %T, want *mcp.ElicitParams", mrtrAnswerID, res.InputRequests[mrtrAnswerID])
			}
			if ir.Message != "which color?" {
				t.Errorf("elicit message = %q, want %q", ir.Message, "which color?")
			}
			if res.RequestState != "s1" {
				t.Errorf("RequestState = %q, want %q", res.RequestState, "s1")
			}

			res, err = cs.CallTool(t.Context(), &mcp.CallToolParams{
				Name:      "ask",
				Arguments: AskInput{Query: "what should I paint?", SessionID: "s1"},
				InputResponses: mcp.InputResponseMap{
					mrtrAnswerID: tt.elicit,
				},
				RequestState: res.RequestState,
			})
			if err != nil {
				t.Fatalf("CallTool() retry error = %v", err)
			}
			if res.NeedsInput() {
				t.Fatalf("NeedsInput() = true after retry, want false")
			}
			if got := textOf(res); got != tt.wantContent {
				t.Errorf("content = %q, want %q", got, tt.wantContent)
			}
		})
	}
}

// TestAskHandler_PlainCallUnaffected: a run that never needs input returns plain content
// with no InputRequests, the common case for every client.
func TestAskHandler_PlainCallUnaffected(t *testing.T) {
	f := &fakeOrch{answered: true} // never pauses
	cs := connect(t, f, nil)

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "ask",
		Arguments: AskInput{Query: "hello", SessionID: "s2"},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if res.NeedsInput() {
		t.Fatalf("NeedsInput() = true, want false")
	}
	if len(res.InputRequests) != 0 {
		t.Errorf("InputRequests = %v, want none", res.InputRequests)
	}
	if got, want := textOf(res), "resolved: hello"; got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
}

// TestAskHandler_NonMRTRClientUnaffected: a client with only an ElicitationHandler still gets one answer
// from a single CallTool, since the go-sdk's default middleware fulfils and retries underneath.
func TestAskHandler_NonMRTRClientUnaffected(t *testing.T) {
	f := &fakeOrch{question: "which color?"}
	cs := connect(t, f, &mcp.ClientOptions{
		ElicitationHandler: func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			if req.Params.Message != "which color?" {
				t.Errorf("elicit message = %q, want %q", req.Params.Message, "which color?")
			}
			return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"answer": "green"}}, nil
		},
	})

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "ask",
		Arguments: AskInput{Query: "what should I paint?", SessionID: "s3"},
	})
	if err != nil {
		t.Fatalf("CallTool() error = %v", err)
	}
	if res.NeedsInput() {
		t.Fatalf("NeedsInput() = true, want false (caller never saw input_required)")
	}
	if got, want := textOf(res), "resolved: green"; got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
}

// TestAskHandler_NoElicitationSupport_ErrorsCleanly: a client with no ElicitationHandler gets a clean
// CallTool error, not a hang or a partial answer.
func TestAskHandler_NoElicitationSupport_ErrorsCleanly(t *testing.T) {
	f := &fakeOrch{question: "which color?"}
	cs := connect(t, f, nil)

	_, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "ask",
		Arguments: AskInput{Query: "what should I paint?", SessionID: "s4"},
	})
	if err == nil {
		t.Fatal("CallTool() error = nil, want an error (client cannot fulfil the elicitation)")
	}
}
