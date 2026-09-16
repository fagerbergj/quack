package replay

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"

	iplugin "github.com/fagerbergj/quack/internal/plugin"
	"github.com/fagerbergj/quack/internal/pluginreg"
	"github.com/fagerbergj/quack/internal/pluginreg/pluginregtest"
	"github.com/fagerbergj/quack/internal/skillsource"
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

// buildLiveSource makes a skill.Source shaped like newSkillSource's output:
// one prefixed sub-source per plugin, from an on-disk skills/ dir per name.
func buildLiveSource(t *testing.T, skillsByPlugin map[string]map[string]string) skill.Source {
	t.Helper()
	var sources []skill.Source
	for plugin, skills := range skillsByPlugin {
		root := t.TempDir()
		for skillName, body := range skills {
			dir := filepath.Join(root, skillName)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		dirFS := os.DirFS(root)
		sources = append(sources, skillsource.Prefixed(plugin, skillsource.NewFileSystemSource(dirFS)))
	}
	return skill.NewMergedSource(sources...)
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

// widgetsAdmitted is the []plugin.Plugin admitPlugins would have produced
// for a "widgets" row with a valid plugin.json (F4: NewSkillSource only
// serves a github row's recorded sha if admission would have allowed it).
func widgetsAdmitted() []iplugin.Plugin { return []iplugin.Plugin{{Name: "widgets"}} }

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
	src, err := NewSkillSource(context.Background(), sess, root, rows, widgetsAdmitted(), nil)
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
	liveSrc := skillsource.NewFileSystemSource(liveFS)
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
	_, err = NewSkillSource(context.Background(), sess, root, rows, widgetsAdmitted(), nil)
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
	_, err = NewSkillSource(context.Background(), sess, root, rows, widgetsAdmitted(), nil)
	if err == nil || !strings.Contains(err.Error(), "widgets") {
		t.Fatalf("err = %v, want a refusal naming widgets", err)
	}
}

func TestNewSkillSource_UnadmittedGithubRowIsSkippedNotServed(t *testing.T) {
	// F4: a row admitPlugins never admitted (no plugin.json) must not be
	// served by replay either, even though the bundle recorded a sha for it.
	root, sha1, _ := fixtureCloneWithSkill(t, "---\nname: dothing\ndescription: v1\n---\nbody v1")
	bundle := writeAgentInvokeJSONL(t, `[{"name":"widgets","sha":"`+sha1+`"}]`)
	sess, err := Load(bundle)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	rows, err := pluginreg.NewFSRegistry(root).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	src, err := NewSkillSource(context.Background(), sess, root, rows, nil /* not admitted */, buildLiveSource(t, nil))
	if err != nil {
		t.Fatalf("NewSkillSource: %v", err)
	}
	if _, err := src.LoadFrontmatter(context.Background(), "widgets:dothing"); err == nil {
		t.Fatal("LoadFrontmatter(unadmitted plugin's skill) = nil error, want ErrSkillNotFound")
	}
}

func TestNewSkillSource_NoPluginsRecordedIsNoOp(t *testing.T) {
	sess := sessionFromEntries(t, nil)
	src, err := NewSkillSource(context.Background(), sess, t.TempDir(), nil, nil, nil)
	if err != nil {
		t.Fatalf("NewSkillSource: %v", err)
	}
	if src != nil {
		t.Fatalf("src = %v, want nil (nothing recorded, caller keeps the live source)", src)
	}
}

func TestNewSkillSource_LocalPluginFallsThroughToLive(t *testing.T) {
	live := buildLiveSource(t, map[string]map[string]string{
		"local-plugin": {"mine": "---\nname: mine\ndescription: local\n---\nbody"},
	})
	rows := []pluginreg.Plugin{{Name: "local-plugin", Source: pluginreg.SourceLocal, Entry: "some/local/path"}}
	bundle := writeAgentInvokeJSONL(t, `[{"name":"local-plugin","sha":""}]`)
	sess, err := Load(bundle)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	src, err := NewSkillSource(context.Background(), sess, t.TempDir(), rows, nil, live)
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

// TestNewSkillSource_ShaLessNonQuackWithNoLiveSkillsSkipsNotRefuses is the
// #1446 carry-over: an admitted, sha-less plugin whose live roster serves
// none of its skills (e.g. an MCP-only plugin, no skills/) must be skipped
// with a warning, not refuse the whole bundle - only the embedded quack
// name is load-bearing enough to hard-refuse on.
func TestNewSkillSource_ShaLessNonQuackWithNoLiveSkillsSkipsNotRefuses(t *testing.T) {
	live := buildLiveSource(t, map[string]map[string]string{
		"other": {"thing": "---\nname: thing\ndescription: d\n---\nbody"},
	})
	rows := []pluginreg.Plugin{
		{Name: "mcp-only", Source: pluginreg.SourceLocal, Entry: "some/local/path"},
		{Name: "other", Source: pluginreg.SourceLocal, Entry: "some/other/path"},
	}
	bundle := writeAgentInvokeJSONL(t, `[{"name":"mcp-only","sha":""},{"name":"other","sha":""}]`)
	sess, err := Load(bundle)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	src, err := NewSkillSource(context.Background(), sess, t.TempDir(), rows, nil, live)
	if err != nil {
		t.Fatalf("NewSkillSource: %v, want no error (mcp-only skipped, not refused)", err)
	}
	if _, err := src.LoadFrontmatter(context.Background(), "other:thing"); err != nil {
		t.Fatalf("LoadFrontmatter(other:thing): %v, want it still served", err)
	}
}

// TestNewSkillSource_ShaLessQuackWithNoLiveSkillsRefuses: the embedded
// quack bundle is always in scope - zero live skills for it is a real gap
// and must still refuse, unlike any other sha-less plugin.
func TestNewSkillSource_ShaLessQuackWithNoLiveSkillsRefuses(t *testing.T) {
	live := buildLiveSource(t, nil) // no skills for any plugin, including quack
	rows := []pluginreg.Plugin{pluginreg.EmbeddedQuackPlugin()}
	bundle := writeAgentInvokeJSONL(t, `[{"name":"quack","sha":""}]`)
	sess, err := Load(bundle)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	_, err = NewSkillSource(context.Background(), sess, t.TempDir(), rows, nil, live)
	if err == nil || !strings.Contains(err.Error(), "quack") {
		t.Fatalf("NewSkillSource err = %v, want a refusal naming quack", err)
	}
}

// TestNewSkillSource_EmbeddedQuackServesFullLiveRoster is review S1: a
// default-config bundle only ever records {"name":"quack","sha":""} (the
// embedded baseline, no clone). Before the fix, sha=="" fell through to a
// per-plugin lookup keyed off []plugin.Plugin, which never includes the
// embedded row (resolveRegistryPlugins skips SourceEmbedded) - the whole
// live roster silently vanished from replay instead of refusing or serving.
func TestNewSkillSource_EmbeddedQuackServesFullLiveRoster(t *testing.T) {
	live := buildLiveSource(t, map[string]map[string]string{
		"quack": {
			"plan-work":       "---\nname: plan-work\ndescription: plan\n---\nbody",
			"format-markdown": "---\nname: format-markdown\ndescription: format\n---\nbody",
		},
	})
	liveNames, err := live.ListFrontmatters(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(liveNames) != 2 {
		t.Fatalf("test setup: live roster = %d names, want 2", len(liveNames))
	}

	rows := []pluginreg.Plugin{pluginreg.EmbeddedQuackPlugin()}
	bundle := writeAgentInvokeJSONL(t, `[{"name":"quack","sha":""}]`)
	sess, err := Load(bundle)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	src, err := NewSkillSource(context.Background(), sess, t.TempDir(), rows, nil, live)
	if err != nil {
		t.Fatalf("NewSkillSource: %v", err)
	}
	replayNames, err := src.ListFrontmatters(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got, want := namesOf(replayNames), namesOf(liveNames); !slices.Equal(got, want) {
		t.Fatalf("replay roster = %v, want equal to live roster %v", got, want)
	}
}

// TestNewSkillSource_ArgumentHintFrontmatterField is review F2: raw ADK
// skill.NewFileSystemSource KnownFields(true)-decodes SKILL.md and errors on
// a field it doesn't know (Claude Code's argument-hint, #1084) - replay must
// go through skillsource.NewFileSystemSource's filter like live does.
func TestNewSkillSource_ArgumentHintFrontmatterField(t *testing.T) {
	body := "---\nname: dothing\ndescription: v1\nargument-hint: <foo>\n---\nbody v1"
	root, sha1, _ := fixtureCloneWithSkill(t, body)

	// Live (clone tip, moved to v2 by fixtureCloneWithSkill): the same
	// skillsource.NewFileSystemSource wrapping resolvedSkillSource uses.
	liveFS := os.DirFS(pluginreg.CloneDir(root, "widgets") + "/skills")
	liveFM, err := skillsource.NewFileSystemSource(liveFS).LoadFrontmatter(context.Background(), "dothing")
	if err != nil {
		t.Fatalf("live LoadFrontmatter (argument-hint field): %v", err)
	}
	if liveFM.Description != "v2" {
		t.Fatalf("live description = %q, want v2", liveFM.Description)
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
	src, err := NewSkillSource(context.Background(), sess, root, rows, widgetsAdmitted(), nil)
	if err != nil {
		t.Fatalf("NewSkillSource: %v", err)
	}
	fm, err := src.LoadFrontmatter(context.Background(), "widgets:dothing")
	if err != nil {
		t.Fatalf("replay LoadFrontmatter (argument-hint field): %v", err)
	}
	if fm.Description != "v1" {
		t.Fatalf("replay description = %q, want v1", fm.Description)
	}
}

func namesOf(fms []*skill.Frontmatter) []string {
	out := make([]string, len(fms))
	for i, fm := range fms {
		out[i] = fm.Name
	}
	sort.Strings(out)
	return out
}
