package replay

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"

	iplugin "github.com/fagerbergj/quack/internal/plugin"
	"github.com/fagerbergj/quack/internal/pluginreg"
	"github.com/fagerbergj/quack/internal/pluginreg/pluginregtest"
)

// writeAgentInvokeJSONL writes one literal agent.invoke ledger line by hand -
// NOT built via ledger.AgentInvokePayload/toEntries - so this test asserts
// the wire shape a real bundle carries (#1427 P4), catching a field-name
// drift the struct-based helpers above would silently absorb.
func writeAgentInvokeJSONL(t *testing.T, pluginsJSON string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "entries.jsonl")
	line := `{"seq":1,"chat_id":"chat-1","node_id":"node-a","agent":"impl","round":"worker-r0","kind":"agent.invoke","at":"2026-01-01T00:00:00Z","payload":{"sent":"[]","received":"[]","plugins":` + pluginsJSON + `}}`
	if err := os.WriteFile(path, []byte(line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func fixtureCloneWithSkill(t *testing.T, body string) (root string, sha1, sha2 string) {
	t.Helper()
	bare, work := pluginregtest.NewFixtureRepo(t)
	run(t, work, "rm", "--quiet", "SKILL.md")
	dir := filepath.Join(work, "skills", "dothing")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, work, "add", ".")
	run(t, work, "commit", "--quiet", "-m", "v1")
	run(t, work, "push", "--quiet", "origin", "main")

	prevURL := pluginreg.RemoteURL
	pluginreg.RemoteURL = func(owner, repo string) string { return bare }
	t.Cleanup(func() { pluginreg.RemoteURL = prevURL })

	root = t.TempDir()
	reg := pluginreg.NewFSRegistry(root)
	e, err := pluginreg.ParseEntry("github:acme/widgets")
	if err != nil {
		t.Fatal(err)
	}
	got, err := reg.Fetch(context.Background(), pluginreg.FromEntry(e))
	if err != nil {
		t.Fatal(err)
	}
	sha1 = got.SHA

	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(strings.Replace(body, "v1", "v2", -1)), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, work, "add", ".")
	run(t, work, "commit", "--quiet", "-m", "v2")
	run(t, work, "push", "--quiet", "origin", "main")
	sha2 = strings.TrimSpace(run(t, work, "rev-parse", "HEAD"))

	if _, err := reg.Fetch(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	return root, sha1, sha2
}

// run mirrors pluginreg's test helper (this package can't import that
// unexported one, so it's a one-line local copy over pluginregtest.RunGit).
func run(t *testing.T, dir string, args ...string) string {
	return pluginregtest.RunGit(t, dir, args...)
}

func TestNewSkillSource_ServesRecordedSHA(t *testing.T) {
	root, sha1, sha2 := fixtureCloneWithSkill(t, "---\nname: dothing\ndescription: v1\n---\nbody v1")
	if sha1 == sha2 {
		t.Fatal("fixture setup: sha1 == sha2")
	}

	bundle := writeAgentInvokeJSONL(t, `[{"name":"widgets","sha":"`+sha1+`"}]`)
	sess, err := Load(bundle)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rows, err := pluginreg.NewFSRegistry(root).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	src, err := NewSkillSource(context.Background(), sess, root, rows, nil)
	if err != nil {
		t.Fatalf("NewSkillSource: %v", err)
	}
	if src == nil {
		t.Fatal("NewSkillSource returned nil, want a source (bundle recorded a plugin)")
	}
	fm, err := src.LoadFrontmatter(context.Background(), "widgets:dothing")
	if err != nil {
		t.Fatalf("LoadFrontmatter: %v", err)
	}
	if fm.Description != "v1" {
		t.Fatalf("description = %q, want v1 (the recorded sha's text, not the live tip)", fm.Description)
	}

	// The live clone, unpinned, now serves the NEW text - replay and live
	// diverge exactly as the epic requires.
	liveFS := os.DirFS(pluginreg.CloneDir(root, "widgets") + "/skills")
	liveSrc := skill.NewFileSystemSource(liveFS)
	liveFM, err := liveSrc.LoadFrontmatter(context.Background(), "dothing")
	if err != nil {
		t.Fatalf("live LoadFrontmatter: %v", err)
	}
	if liveFM.Description != "v2" {
		t.Fatalf("live description = %q, want v2", liveFM.Description)
	}
}

func TestNewSkillSource_DeletedCloneRefuses(t *testing.T) {
	root, sha1, _ := fixtureCloneWithSkill(t, "---\nname: dothing\ndescription: v1\n---\nbody v1")
	if err := os.RemoveAll(pluginreg.CloneDir(root, "widgets")); err != nil {
		t.Fatal(err)
	}
	bundle := writeAgentInvokeJSONL(t, `[{"name":"widgets","sha":"`+sha1+`"}]`)
	sess, err := Load(bundle)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rows, err := pluginreg.NewFSRegistry(root).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewSkillSource(context.Background(), sess, root, rows, nil)
	if err == nil || !strings.Contains(err.Error(), "widgets") {
		t.Fatalf("err = %v, want a refusal naming widgets", err)
	}
}

func TestNewSkillSource_UnknownSHARefuses(t *testing.T) {
	root, _, _ := fixtureCloneWithSkill(t, "---\nname: dothing\ndescription: v1\n---\nbody v1")
	bundle := writeAgentInvokeJSONL(t, `[{"name":"widgets","sha":"`+strings.Repeat("f", 40)+`"}]`)
	sess, err := Load(bundle)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rows, err := pluginreg.NewFSRegistry(root).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = NewSkillSource(context.Background(), sess, root, rows, nil)
	if err == nil || !strings.Contains(err.Error(), "widgets") {
		t.Fatalf("err = %v, want a refusal naming widgets", err)
	}
}

func TestNewSkillSource_NoPluginsRecordedIsNoOp(t *testing.T) {
	sess := sessionFromEntries(t, nil)
	src, err := NewSkillSource(context.Background(), sess, t.TempDir(), nil, nil)
	if err != nil {
		t.Fatalf("NewSkillSource: %v", err)
	}
	if src != nil {
		t.Fatalf("src = %v, want nil (nothing recorded, caller keeps the live source)", src)
	}
}

func TestNewSkillSource_LocalEmbeddedFallsThroughToLive(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "skills", "mine")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: mine\ndescription: local\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	bundle := writeAgentInvokeJSONL(t, `[{"name":"local-plugin","sha":""}]`)
	sess, err := Load(bundle)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	live := []iplugin.Plugin{{Name: "local-plugin", SkillsDir: filepath.Join(dir, "skills")}}
	src, err := NewSkillSource(context.Background(), sess, t.TempDir(), nil, live)
	if err != nil {
		t.Fatalf("NewSkillSource: %v", err)
	}
	fm, err := src.LoadFrontmatter(context.Background(), "local-plugin:mine")
	if err != nil {
		t.Fatalf("LoadFrontmatter: %v", err)
	}
	if fm.Description != "local" {
		t.Fatalf("description = %q, want local", fm.Description)
	}
}
