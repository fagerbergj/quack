package agent

import (
	"strings"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// toolResultModel is toolResult but at role MODEL — the role production workers
// actually persist their tool results under (the orchestrator uses role user).
func toolResultModel(name, id string, n int) *genai.Content {
	return &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{Name: name, ID: id, Response: map[string]any{"result": strings.Repeat("x", n)}},
	}}}
}

// TestNoBlankedToolResultsSurviveCompaction_ModelRole is the production-role
// variant of the anti-amnesia invariant: a surviving tool result must be intact,
// never a placeholder — even when the result is role MODEL (worker) not role USER
// (orchestrator). If this fails while the role-user version passes, compaction
// ejects worker tool results and the model re-reads the same file/command forever.
func TestNoBlankedToolResultsSurviveCompaction_ModelRole(t *testing.T) {
	llm := &fakeLLM{text: "## Goal\n- compacted"}
	cb := compactionCallback(Compaction{Summarizer: llm, ContextWindow: 80_000, Enabled: true})

	contents := []*genai.Content{textContent(genai.RoleUser, "task")}
	for i := 0; i < 12; i++ {
		contents = append(contents, toolCall("read_file", "c"))
		contents = append(contents, toolResultModel("read_file", "c", 40_000))
	}
	req := &model.LLMRequest{Contents: contents}
	if _, err := cb(newFakeCtx(), req); err != nil {
		t.Fatalf("callback err: %v", err)
	}

	intact := strings.Repeat("x", 40_000)
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			fr := p.FunctionResponse
			if fr == nil {
				continue
			}
			if got, _ := fr.Response["result"].(string); got != intact {
				t.Fatalf("a role-MODEL tool result survived but was BLANKED (%q) — production worker results are ejected while role-user survive; the model sees it read the file, content gone, re-reads forever", truncate(got, 80))
			}
		}
	}
	if llm.calls != 1 {
		t.Fatalf("older tool outputs discarded without summarising: summariser calls=%d; want 1", llm.calls)
	}
}
