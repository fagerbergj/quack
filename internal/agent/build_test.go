package agent

import (
	"context"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/skilltoolset"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/skillsource"
)

// writeSkill drops a minimal SKILL.md fixture under dir/name.
func writeSkill(t *testing.T, dir, name, description string) {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\nInstructions for " + name + ".\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// captureSystemInstructionModel records the system instruction ADK assembled
// for the one turn it answers, then ends the turn with plain text.
type captureSystemInstructionModel struct{ sysInstruction string }

func (m *captureSystemInstructionModel) Name() string { return "capture" }

func (m *captureSystemInstructionModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if req.Config != nil && req.Config.SystemInstruction != nil {
			var b strings.Builder
			for _, p := range req.Config.SystemInstruction.Parts {
				if p != nil {
					b.WriteString(p.Text)
				}
			}
			m.sysInstruction = b.String()
		}
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "ok"}}},
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil)
	}
}

// TestBuild_SkillRosterListedOnce guards finding A4: every ADK-native agent
// (built via Build/BuildChat) carries a SkillToolset in its Toolsets, whose
// own ProcessRequest already renders the skill roster into the request. If
// promptbuilder.Agent's InstructionProvider also rendered it, the roster
// would appear twice in every worker request - on the largest cacheable
// prefix in the system. Build must pass nil skills to promptbuilder.Agent so
// the toolset is the roster's only source.
func TestBuild_SkillRosterListedOnce(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "format-markdown", "Reformats markdown for clean rendering.")
	writeSkill(t, dir, "review-code", "Reviews a diff for correctness.")
	src := skillsource.NewFileSystemSource(os.DirFS(dir))

	ts, err := skilltoolset.New(context.Background(), skilltoolset.Config{Source: src})
	if err != nil {
		t.Fatalf("skilltoolset: %v", err)
	}
	fms, err := src.ListFrontmatters(context.Background())
	if err != nil {
		t.Fatalf("frontmatters: %v", err)
	}
	if len(fms) != 2 {
		t.Fatalf("test fixture: got %d skills, want 2", len(fms))
	}

	b := &Bundle{Card: Card{Name: "tester", Description: "a test agent"}, Prompt: "Do the task."}
	capture := &captureSystemInstructionModel{}
	ag, err := Build(b, capture, nil, []tool.Toolset{ts}, "", fms, "", nil)
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	r, err := runner.New(runner.Config{AppName: "test", Agent: ag, SessionService: session.InMemoryService(), AutoCreateSession: true})
	if err != nil {
		t.Fatalf("runner: %v", err)
	}
	task := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "hello"}}}
	for _, err := range r.Run(context.Background(), "u", "s", task, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}

	if capture.sysInstruction == "" {
		t.Fatal("model never saw a system instruction")
	}
	for _, name := range []string{"format-markdown", "review-code"} {
		if n := strings.Count(capture.sysInstruction, name); n != 1 {
			t.Errorf("skill %q appears %d times in the assembled system prompt, want exactly 1:\n%s", name, n, capture.sysInstruction)
		}
	}
}
