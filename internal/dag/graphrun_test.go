// Package dag_test holds shared stub-model and graph-run helpers used by
// this package's external tests (guard_a2a_test.go and others) that need the
// real dag.Executor/gate machinery.
package dag_test

import (
	"context"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
)

// local stub-model helpers (mirrors dag's own gCall/gText/gSysText/gUserText/
// gHasTool - duplicated because those are unexported in `package dag` and this
// is an external test package)

func atText(s string) *model.LLMResponse {
	return &model.LLMResponse{
		Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: s}}},
		FinishReason: genai.FinishReasonStop,
		TurnComplete: true,
	}
}

func atCall(name string, args map[string]any) *model.LLMResponse {
	return &model.LLMResponse{
		Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
			FunctionCall: &genai.FunctionCall{Name: name, Args: args},
		}}},
		FinishReason: genai.FinishReasonStop,
		TurnComplete: true,
	}
}

func atHasTool(req *model.LLMRequest, name string) bool {
	if req.Config == nil {
		return false
	}
	for _, tl := range req.Config.Tools {
		if tl == nil {
			continue
		}
		for _, fd := range tl.FunctionDeclarations {
			if fd != nil && fd.Name == name {
				return true
			}
		}
	}
	return false
}

func atAllText(req *model.LLMRequest) string {
	var b strings.Builder
	for _, c := range req.Contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && !p.Thought && p.Text != "" {
				b.WriteString(p.Text)
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}

// runGraphChatID is the chat/session id runGraph runs every plan under, and ALSO
// the per-chat WORKSPACE scope the run's tools resolve paths through
// (<root>/<user>/<runGraphChatID>/…, derived by tools.chatScopeFromContext) - a test seeding a jail fixture for those tools must write it under this SAME id, hence one named constant instead of a literal at each site.
const runGraphChatID = "s"

// runGraphNodeID is the id of the single node every runGraph test plan uses. Since
// #198 a node's tools DEFAULT their cwd to the node's OWN dir (<chat>/<node>/), so a
// fixture those tools must act on has to be seeded there - not at the chat root: a chat-root fixture leaves the tool resolving a nonexistent path, and a guarded delete then never completes - exactly how TestGuardConfirm_OverA2A hung for the full 10-minute CI timeout.
const runGraphNodeID = "n1"

// runGraph runs plan via the REAL dag.Executor (RunPlanAsGraph - the native
// graph path production uses), collecting the SSE events and node outputs.
func runGraph(t *testing.T, worker adkagent.Agent, judgeModel model.LLM, sessions session.Service, plan dag.Plan, content *genai.Content, resumeNodes []string) (paused bool, outputs map[string]string, events []stream.SSEEvent) {
	t.Helper()
	ex := dag.NewExecutor(sessions, map[string]adkagent.Agent{"blk": worker}, nil,
		vetting.NewJudgeFactory(judgeModel, nil, nil),
		func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 2} }, nil)
	outputs = map[string]string{}
	yield := func(ev stream.SSEEvent, _ error) bool { events = append(events, ev); return true }
	p, err := ex.RunPlanAsGraph(context.Background(), plan, "quack-test", "u", runGraphChatID, content, yield, outputs, resumeNodes)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return p, outputs, events
}
