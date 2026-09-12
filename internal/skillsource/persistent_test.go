package skillsource

import (
	"context"
	"encoding/json"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/skilltoolset"
	"google.golang.org/adk/v2/tool/skilltoolset/skill"
	"google.golang.org/adk/v2/tool/toolconfirmation"
)

// fakeState is a minimal in-memory session.State, mirroring internal/tools'
// own test double for the same interface.
type fakeState struct{ m map[string]any }

func (s *fakeState) Get(k string) (any, error) {
	if v, ok := s.m[k]; ok {
		return v, nil
	}
	return nil, session.ErrStateKeyNotExist
}
func (s *fakeState) Set(k string, v any) error { s.m[k] = v; return nil }
func (s *fakeState) All() iter.Seq2[string, any] {
	return func(yield func(string, any) bool) {
		for k, v := range s.m {
			if !yield(k, v) {
				return
			}
		}
	}
}

// fakeCtx is an agent.Context double; two instances sharing state simulate
// two calls within one session.
type fakeCtx struct {
	adkagent.StrictContextMock
	invocationID string
	state        *fakeState
}

func newFakeCtx(invocationID string, state *fakeState) *fakeCtx {
	if state == nil {
		state = &fakeState{m: map[string]any{}}
	}
	return &fakeCtx{StrictContextMock: adkagent.StrictContextMock{Ctx: context.Background()}, invocationID: invocationID, state: state}
}

func (c *fakeCtx) InvocationID() string                                 { return c.invocationID }
func (c *fakeCtx) ReadonlyState() session.ReadonlyState                 { return c.state }
func (c *fakeCtx) State() session.State                                 { return c.state }
func (c *fakeCtx) ToolConfirmation() *toolconfirmation.ToolConfirmation { return nil }

// findTool locates a tool by name among a Toolset's Tools(), or fails the test.
func findTool(t *testing.T, ts tool.Toolset, name string) tool.Tool {
	t.Helper()
	tools, err := ts.Tools(newFakeCtx("inv0", nil))
	if err != nil {
		t.Fatalf("Tools(): %v", err)
	}
	for _, tt := range tools {
		if tt.Name() == name {
			return tt
		}
	}
	t.Fatalf("tool %q not found among %d tools", name, len(tools))
	return nil
}

func runTool(t *testing.T, tl tool.Tool, ctx adkagent.Context, args map[string]any) map[string]any {
	t.Helper()
	rt, ok := tl.(interface {
		Run(ctx adkagent.Context, args any) (map[string]any, error)
	})
	if !ok {
		t.Fatalf("tool %q does not implement Run", tl.Name())
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	var decoded any
	if err := json.Unmarshal(argsJSON, &decoded); err != nil {
		t.Fatal(err)
	}
	out, err := rt.Run(ctx, decoded)
	if err != nil {
		t.Fatalf("Run(%q): %v", tl.Name(), err)
	}
	return out
}

func newTestSource(t *testing.T, skills map[string]string) skill.Source {
	t.Helper()
	dir := t.TempDir()
	for name, body := range skills {
		writeSkill(t, dir, name, name+" description", body)
	}
	return skill.NewFileSystemSource(os.DirFS(dir))
}

func newPersistent(t *testing.T, skills map[string]string) tool.Toolset {
	t.Helper()
	src := newTestSource(t, skills)
	inner, err := skilltoolset.New(context.Background(), skilltoolset.Config{Source: src})
	if err != nil {
		t.Fatalf("skilltoolset.New: %v", err)
	}
	return Persistent(inner, src)
}

func systemInstructionText(req *model.LLMRequest) string {
	if req.Config == nil || req.Config.SystemInstruction == nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range req.Config.SystemInstruction.Parts {
		if p != nil {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// TestLoadedSkillRidesInstructionNextCall: the loading call's own tool result
// carries the body; a later call finds it in the instruction, not Contents.
func TestLoadedSkillRidesInstructionNextCall(t *testing.T) {
	ts := newPersistent(t, map[string]string{"plan-work": "the plan-work body"})
	state := &fakeState{m: map[string]any{}}

	loadCtx := newFakeCtx("inv1", state)
	loadResult := runTool(t, findTool(t, ts, "load_skill"), loadCtx, map[string]any{"name": "plan-work"})
	if got, _ := loadResult["instructions"].(string); strings.TrimSpace(got) != "the plan-work body" {
		t.Fatalf("load call's own tool result = %q, want the full body", got)
	}

	nextReq := &model.LLMRequest{}
	if err := ts.(interface {
		ProcessRequest(adkagent.Context, *model.LLMRequest) error
	}).ProcessRequest(newFakeCtx("inv2", state), nextReq); err != nil {
		t.Fatalf("ProcessRequest: %v", err)
	}
	if instr := systemInstructionText(nextReq); !strings.Contains(instr, "the plan-work body") {
		t.Fatalf("next call's system instruction = %q, want it to contain the loaded body", instr)
	}
	if len(nextReq.Contents) != 0 {
		t.Fatalf("ProcessRequest must not touch Contents, got %v", nextReq.Contents)
	}
}

// TestLoadedSkillNotDuplicatedInLoadingTurn: the loading invocation's own
// request must not also get the body via the instruction - it is already in that turn's tool result.
func TestLoadedSkillNotDuplicatedInLoadingTurn(t *testing.T) {
	ts := newPersistent(t, map[string]string{"plan-work": "the plan-work body"})
	state := &fakeState{m: map[string]any{}}
	ctx := newFakeCtx("inv1", state)

	runTool(t, findTool(t, ts, "load_skill"), ctx, map[string]any{"name": "plan-work"})

	sameReq := &model.LLMRequest{}
	if err := ts.(interface {
		ProcessRequest(adkagent.Context, *model.LLMRequest) error
	}).ProcessRequest(ctx, sameReq); err != nil {
		t.Fatalf("ProcessRequest: %v", err)
	}
	if instr := systemInstructionText(sameReq); strings.Contains(instr, "the plan-work body") {
		t.Fatalf("same-invocation ProcessRequest must not repeat the body, got instruction %q", instr)
	}
}

// TestLoadedSkillsSortedByName: order must be deterministic or the shared
// prefix (and vLLM's cache of it) shifts on every subsequent load.
func TestLoadedSkillsSortedByName(t *testing.T) {
	ts := newPersistent(t, map[string]string{"zeta": "zeta body", "alpha": "alpha body"})
	state := &fakeState{m: map[string]any{}}

	runTool(t, findTool(t, ts, "load_skill"), newFakeCtx("inv1", state), map[string]any{"name": "zeta"})
	runTool(t, findTool(t, ts, "load_skill"), newFakeCtx("inv2", state), map[string]any{"name": "alpha"})

	req := &model.LLMRequest{}
	if err := ts.(interface {
		ProcessRequest(adkagent.Context, *model.LLMRequest) error
	}).ProcessRequest(newFakeCtx("inv3", state), req); err != nil {
		t.Fatalf("ProcessRequest: %v", err)
	}
	instr := systemInstructionText(req)
	alphaAt, zetaAt := strings.Index(instr, "alpha body"), strings.Index(instr, "zeta body")
	if alphaAt == -1 || zetaAt == -1 || alphaAt > zetaAt {
		t.Fatalf("want alpha before zeta in instruction, got %q", instr)
	}
}

// TestReloadingASkillDoesNotResendItsBody: reloading an already-loaded skill
// returns a short acknowledgement, not the body again.
func TestReloadingASkillDoesNotResendItsBody(t *testing.T) {
	ts := newPersistent(t, map[string]string{"plan-work": "the plan-work body"})
	state := &fakeState{m: map[string]any{}}

	first := runTool(t, findTool(t, ts, "load_skill"), newFakeCtx("inv1", state), map[string]any{"name": "plan-work"})
	if got, _ := first["instructions"].(string); strings.TrimSpace(got) != "the plan-work body" {
		t.Fatalf("first load = %q, want the full body", got)
	}

	second := runTool(t, findTool(t, ts, "load_skill"), newFakeCtx("inv2", state), map[string]any{"name": "plan-work"})
	if got, _ := second["instructions"].(string); strings.TrimSpace(got) == "the plan-work body" || !strings.Contains(got, "plan-work") {
		t.Fatalf("reload = %q, want a short acknowledgement naming the skill, not the body again", got)
	}
}

// TestListSkillsAndLoadSkillResourceStillWork: wrapping only load_skill must
// not break the other two skill tools.
func TestListSkillsAndLoadSkillResourceStillWork(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "plan-work", "plans work", "body")
	if err := os.MkdirAll(filepath.Join(dir, "plan-work", "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plan-work", "references", "notes.md"), []byte("resource content"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := skill.NewFileSystemSource(os.DirFS(dir))
	inner, err := skilltoolset.New(context.Background(), skilltoolset.Config{Source: src})
	if err != nil {
		t.Fatalf("skilltoolset.New: %v", err)
	}
	ts := Persistent(inner, src)
	ctx := newFakeCtx("inv1", nil)

	listOut := runTool(t, findTool(t, ts, "list_skills"), ctx, map[string]any{})
	if skills, _ := listOut["skills"].(string); !strings.Contains(skills, "plan-work") {
		t.Fatalf("list_skills output = %v, want it to mention plan-work", listOut)
	}

	resOut := runTool(t, findTool(t, ts, "load_skill_resource"), ctx, map[string]any{
		"skill_name": "plan-work", "resource_path": "references/notes.md",
	})
	if content, _ := resOut["content"].(string); content != "resource content" {
		t.Fatalf("load_skill_resource content = %q, want %q", content, "resource content")
	}
}
