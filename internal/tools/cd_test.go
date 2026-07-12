package tools

import (
	"context"
	"iter"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// fakeState is an in-memory session.State for exercising the cwd round-trip.
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

// fakeCtx embeds StrictContextMock and serves a real State so cwd persists
// across calls (mirrors internal/agent/compaction_test.go's fakeCtx).
type fakeCtx struct {
	adkagent.StrictContextMock
	state *fakeState
}

func newFakeCtx() *fakeCtx {
	return &fakeCtx{
		StrictContextMock: adkagent.StrictContextMock{Ctx: context.Background()},
		state:             &fakeState{m: map[string]any{}},
	}
}

func (c *fakeCtx) UserContent() *genai.Content          { return nil }
func (c *fakeCtx) InvocationID() string                 { return "inv" }
func (c *fakeCtx) AgentName() string                    { return "test" }
func (c *fakeCtx) ReadonlyState() session.ReadonlyState { return c.state }
func (c *fakeCtx) UserID() string                       { return "u" }
func (c *fakeCtx) AppName() string                      { return "app" }
func (c *fakeCtx) SessionID() string                    { return "sess" }
func (c *fakeCtx) Branch() string                       { return "" }
func (c *fakeCtx) Artifacts() adkagent.Artifacts        { return nil }
func (c *fakeCtx) State() session.State                 { return c.state }

func skillFile(name, desc string) string {
	return "---\nname: " + name + "\ndescription: " + desc + "\n---\n\nbody\n"
}

// ---------------------------------------------------------------------------
// nearest AGENTS.md / CLAUDE.md
// ---------------------------------------------------------------------------

func TestCdNearestAgentsWins(t *testing.T) {
	b := newTestBinding(t, "u1")
	writeUserFile(t, b, "repo/AGENTS.md", "root instructions")
	writeUserFile(t, b, "repo/sub/AGENTS.md", "sub instructions")
	writeUserFile(t, b, "repo/sub/file.go", "x")

	ctx := newFakeCtx()
	res, err := b.withCwd(ctx).cd(ctx, cdArgs{Dir: "repo/sub"})
	if err != nil {
		t.Fatal(err)
	}
	if res.InstructionsPath != "repo/sub/AGENTS.md" {
		t.Errorf("InstructionsPath = %q, want repo/sub/AGENTS.md", res.InstructionsPath)
	}
	if res.Instructions != "sub instructions" {
		t.Errorf("Instructions = %q, want sub instructions (closest wins)", res.Instructions)
	}
}

func TestCdClaudeFallback(t *testing.T) {
	b := newTestBinding(t, "u1")
	writeUserFile(t, b, "repo/CLAUDE.md", "claude instructions")
	writeUserFile(t, b, "repo/readme.md", "x")

	ctx := newFakeCtx()
	res, err := b.withCwd(ctx).cd(ctx, cdArgs{Dir: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	if res.InstructionsPath != "repo/CLAUDE.md" || res.Instructions != "claude instructions" {
		t.Errorf("got path=%q content=%q, want CLAUDE.md fallback", res.InstructionsPath, res.Instructions)
	}
}

func TestCdAgentsBeatsClaudeSameDir(t *testing.T) {
	b := newTestBinding(t, "u1")
	writeUserFile(t, b, "repo/AGENTS.md", "agents wins")
	writeUserFile(t, b, "repo/CLAUDE.md", "claude loses")

	ctx := newFakeCtx()
	res, err := b.withCwd(ctx).cd(ctx, cdArgs{Dir: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Instructions != "agents wins" {
		t.Errorf("Instructions = %q, want AGENTS.md to win over CLAUDE.md", res.Instructions)
	}
}

func TestCdNoInstructionsFound(t *testing.T) {
	b := newTestBinding(t, "u1")
	writeUserFile(t, b, "repo/main.go", "x")

	ctx := newFakeCtx()
	res, err := b.withCwd(ctx).cd(ctx, cdArgs{Dir: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	if res.InstructionsPath != "" || res.Instructions != "" {
		t.Errorf("expected no instructions, got path=%q", res.InstructionsPath)
	}
	if !strings.Contains(res.Note, "no project instructions") {
		t.Errorf("Note = %q, want a 'no project instructions' note", res.Note)
	}
}

func TestCdJailEscapeBlocked(t *testing.T) {
	b := newTestBinding(t, "u1")
	ctx := newFakeCtx()
	if _, err := b.withCwd(ctx).cd(ctx, cdArgs{Dir: "../../etc"}); err == nil {
		t.Fatal("cd ../../etc should escape the jail, got nil error")
	}
}

func TestCdNotADirectory(t *testing.T) {
	b := newTestBinding(t, "u1")
	writeUserFile(t, b, "repo/file.txt", "x")
	ctx := newFakeCtx()
	if _, err := b.withCwd(ctx).cd(ctx, cdArgs{Dir: "repo/file.txt"}); err == nil {
		t.Fatal("cd into a file should error")
	}
}

// ---------------------------------------------------------------------------
// project-skill discovery
// ---------------------------------------------------------------------------

func TestCdDiscoversProjectSkills(t *testing.T) {
	b := newTestBinding(t, "u1")
	writeUserFile(t, b, "repo/.agents/skills/alpha/SKILL.md", skillFile("alpha", "the alpha skill"))
	writeUserFile(t, b, "repo/.claude/skills/beta/SKILL.md", skillFile("beta", "the beta skill"))
	writeUserFile(t, b, "repo/main.go", "x")

	ctx := newFakeCtx()
	res, err := b.withCwd(ctx).cd(ctx, cdArgs{Dir: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range res.Skills {
		got[s.Name] = s.Description
	}
	if got["alpha"] != "the alpha skill" || got["beta"] != "the beta skill" {
		t.Errorf("skills = %+v, want alpha + beta discovered", res.Skills)
	}
}

func TestCdFromSubdirStillFindsRepoSkills(t *testing.T) {
	b := newTestBinding(t, "u1")
	writeUserFile(t, b, "repo/.agents/skills/alpha/SKILL.md", skillFile("alpha", "a"))
	writeUserFile(t, b, "repo/pkg/deep/x.go", "x")

	ctx := newFakeCtx()
	res, err := b.withCwd(ctx).cd(ctx, cdArgs{Dir: "repo/pkg/deep"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Skills) != 1 || res.Skills[0].Name != "alpha" {
		t.Errorf("skills = %+v, want the repo's alpha skill even from a nested cwd", res.Skills)
	}
}

// ---------------------------------------------------------------------------
// cwd is set + persisted + honoured by later tools
// ---------------------------------------------------------------------------

func TestCdSetsCwdInState(t *testing.T) {
	b := newTestBinding(t, "u1")
	writeUserFile(t, b, "repo/main.go", "x")
	ctx := newFakeCtx()

	res, err := b.withCwd(ctx).cd(ctx, cdArgs{Dir: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Dir != "repo" {
		t.Errorf("res.Dir = %q, want repo", res.Dir)
	}
	v, err := ctx.state.Get(CwdKey)
	if err != nil || v.(string) != "repo" {
		t.Errorf("state[%s] = %v (err %v), want repo", CwdKey, v, err)
	}
}

func TestCdComposesLikeShell(t *testing.T) {
	b := newTestBinding(t, "u1")
	writeUserFile(t, b, "repo/sub/main.go", "x")
	ctx := newFakeCtx()

	if _, err := b.withCwd(ctx).cd(ctx, cdArgs{Dir: "repo"}); err != nil {
		t.Fatal(err)
	}
	// A second cd with a relative arg composes onto the first (repo → repo/sub).
	res, err := b.withCwd(ctx).cd(ctx, cdArgs{Dir: "sub"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Dir != "repo/sub" {
		t.Errorf("res.Dir = %q, want repo/sub (cd composes like a shell)", res.Dir)
	}
	// A leading "/" resets to the workspace root.
	res, err = b.withCwd(ctx).cd(ctx, cdArgs{Dir: "/"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Dir != "." {
		t.Errorf("res.Dir = %q, want . (cd / returns to root)", res.Dir)
	}
}

func TestCwdHonouredByReadFileAndListDir(t *testing.T) {
	b := newTestBinding(t, "u1")
	writeUserFile(t, b, "repo/hello.txt", "hi from repo")
	ctx := newFakeCtx()

	if _, err := b.withCwd(ctx).cd(ctx, cdArgs{Dir: "repo"}); err != nil {
		t.Fatal(err)
	}
	// A RELATIVE path now resolves against the cwd, not the jail root.
	rf, err := b.withCwd(ctx).readFile(readFileArgs{Path: "hello.txt"})
	if err != nil {
		t.Fatalf("read_file relative to cwd: %v", err)
	}
	if rf.Content != "hi from repo" {
		t.Errorf("read_file content = %q, want the repo file", rf.Content)
	}
	// list_dir "." lists the cwd, with entries re-rooted to the cwd.
	ld, err := b.withCwd(ctx).listDir(listDirArgs{Path: ""})
	if err != nil {
		t.Fatalf("list_dir: %v", err)
	}
	found := false
	for _, e := range ld.Entries {
		if e.Path == "hello.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("list_dir entries = %+v, want cwd-relative hello.txt", ld.Entries)
	}
}

func TestCwdLeadingSlashEscapesToRoot(t *testing.T) {
	b := newTestBinding(t, "u1")
	writeUserFile(t, b, "repo/hello.txt", "in repo")
	writeUserFile(t, b, "top.txt", "at root")
	ctx := newFakeCtx()

	if _, err := b.withCwd(ctx).cd(ctx, cdArgs{Dir: "repo"}); err != nil {
		t.Fatal(err)
	}
	// "/top.txt" addresses the workspace root, ignoring the cwd.
	rf, err := b.withCwd(ctx).readFile(readFileArgs{Path: "/top.txt"})
	if err != nil {
		t.Fatalf("read_file /top.txt: %v", err)
	}
	if rf.Content != "at root" {
		t.Errorf("content = %q, want the root file via leading-slash escape", rf.Content)
	}
}

func TestCwdCannotEscapeJail(t *testing.T) {
	b := newTestBinding(t, "u1")
	writeUserFile(t, b, "repo/x.txt", "x")
	ctx := newFakeCtx()
	if _, err := b.withCwd(ctx).cd(ctx, cdArgs{Dir: "repo"}); err != nil {
		t.Fatal(err)
	}
	// cwd=repo + a climbing relative path must still be caught by the jail.
	if _, err := b.withCwd(ctx).readFile(readFileArgs{Path: "../../../etc/passwd"}); err == nil {
		t.Fatal("cwd + climbing path should escape the jail, got nil error")
	}
}

func TestCwdHonouredByRunCommand(t *testing.T) {
	b := newTestBinding(t, "u1")
	writeUserFile(t, b, "repo/marker.txt", "x")
	ctx := newFakeCtx()
	if _, err := b.withCwd(ctx).cd(ctx, cdArgs{Dir: "repo"}); err != nil {
		t.Fatal(err)
	}
	// dir="" now means the cwd; pwd should print a path ending in /repo.
	res, err := b.withCwd(ctx).runCommand(runCommandArgs{Dir: "", Command: "pwd"})
	if err != nil {
		t.Fatalf("run_command pwd: %v", err)
	}
	if !strings.HasSuffix(strings.TrimSpace(res.Output), "/repo") {
		t.Errorf("pwd = %q, want a path ending in /repo (ran in the cwd)", strings.TrimSpace(res.Output))
	}
}

// cwdFromState / joinCwd unit coverage (the resolution primitives).

func TestCwdFromStateFallsBackToRoot(t *testing.T) {
	if got := cwdFromState(newFakeCtx()); got != "" {
		t.Errorf("cwdFromState with no cd = %q, want empty (root)", got)
	}
	if got := cwdFromState(nil); got != "" {
		t.Errorf("cwdFromState(nil) = %q, want empty", got)
	}
}

func TestJoinCwd(t *testing.T) {
	cases := []struct{ cwd, path, want string }{
		{"", "a/b", "a/b"},                    // no cwd: unchanged
		{"repo", "src/x.go", "repo/src/x.go"}, // relative joins onto cwd
		{"repo", "/top", "top"},               // leading slash: root-relative, cwd ignored
		{"repo", "..", "."},                   // climbing composes (jail re-checks later)
	}
	for _, c := range cases {
		if got := joinCwd(c.cwd, c.path); got != c.want {
			t.Errorf("joinCwd(%q,%q) = %q, want %q", c.cwd, c.path, got, c.want)
		}
	}
}
