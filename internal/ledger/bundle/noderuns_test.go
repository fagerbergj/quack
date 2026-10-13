package bundle

import "testing"

func TestNodeRuns_TaskAnswerAndMetadata(t *testing.T) {
	path := writeJSONL(t, []entry{
		chat(t0(), "node-1", "code-reviewer", "worker-r0", "worker-model", map[string]any{
			"gen_ai.input.messages":   `[{"role":"user","parts":[{"text":"review this PR"}]}]`,
			"gen_ai.output.messages":  `{"role":"model","parts":[{"text":"looks good"}]}`,
			"quack.prompt.source":     "langfuse",
			"quack.prompt.version_id": "3",
		}),
		// A non-matching agent must not show up in a NodeRuns(code-reviewer) result.
		chat(t0(), "node-2", "synthesizer", "worker-r0", "worker-model", map[string]any{
			"gen_ai.input.messages":  `[{"role":"user","parts":[{"text":"summarize"}]}]`,
			"gen_ai.output.messages": `{"role":"model","parts":[{"text":"summary"}]}`,
		}),
	})
	sess, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	runs := sess.NodeRuns(map[string]bool{"code-reviewer": true})
	if len(runs) != 1 {
		t.Fatalf("want exactly one code-reviewer run, got %d: %+v", len(runs), runs)
	}
	run, ok := runs[StreamKey{Node: "node-1", Agent: "code-reviewer", Round: "worker-r0"}]
	if !ok {
		t.Fatalf("missing node-1's run, got %+v", runs)
	}
	if run.Task != "review this PR" {
		t.Errorf("Task = %q", run.Task)
	}
	if run.Answer != "looks good" {
		t.Errorf("Answer = %q", run.Answer)
	}
	if run.PromptSource != "langfuse" || run.PromptVersionID != "3" {
		t.Errorf("PromptSource/PromptVersionID = %q/%q", run.PromptSource, run.PromptVersionID)
	}
}

func TestNodeRuns_EmptyForRootStreamAndNoMatch(t *testing.T) {
	path := writeJSONL(t, []entry{
		rootChat(t0(), "orch-model", map[string]any{
			"gen_ai.input.messages":  `[{"role":"user","parts":[{"text":"top level"}]}]`,
			"gen_ai.output.messages": `{"role":"model","parts":[{"text":"top answer"}]}`,
		}),
	})
	sess, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if runs := sess.NodeRuns(map[string]bool{"code-reviewer": true}); len(runs) != 0 {
		t.Fatalf("root stream must never appear in NodeRuns, got %+v", runs)
	}
}

// A native node sees its task as ADK renders the gate's message: "run", "For context:", "[quack-gate] said: <task>".
// The run's Task is the gate-built task alone, so replaying it doesn't nest that framing twice.
func TestNodeRuns_TaskDropsADKFraming(t *testing.T) {
	path := writeJSONL(t, []entry{chat(t0(), "n1", "synthesizer", "worker-r0", "m", map[string]any{
		"gen_ai.input.messages":  `[{"role":"user","parts":[{"text":"run"},{"text":"For context:"},{"text":"[quack-gate] said: BACKGROUND x\n\nYOUR TASK - do this, and ONLY this:\nsynthesize"}]}]`,
		"gen_ai.output.messages": `{"role":"model","parts":[{"text":"done"}]}`,
	})})
	sess, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	runs := sess.NodeRuns(map[string]bool{"synthesizer": true})
	if len(runs) != 1 {
		t.Fatalf("NodeRuns = %+v, want one synthesizer run", runs)
	}
	for _, run := range runs {
		if want := "BACKGROUND x\n\nYOUR TASK - do this, and ONLY this:\nsynthesize"; run.Task != want {
			t.Fatalf("Task = %q, want the gate-built task %q", run.Task, want)
		}
	}
}
