package agent

import (
	"context"
	"flag"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"

	"github.com/fagerbergj/quack/internal/bundledir"
	"github.com/fagerbergj/quack/internal/promptbuilder"
)

// updateGolden regenerates testdata/prompts.
var updateGolden = flag.Bool("update-golden", false, "rewrite the prompt golden files")

// todayLine: the Environment layer's only non-deterministic input.
var todayLine = regexp.MustCompile(`Today is [^\n]*\.`)

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	got = todayLine.ReplaceAllString(got, "Today is <DATE>.")
	path := filepath.Join("testdata", "prompts", name)
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v (run go test -run Golden -update-golden)", path, err)
	}
	if got != string(want) {
		t.Errorf("%s: assembled prompt changed\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// pluginAgentBundleDirs lists every .agents/plugins/<name>/agents/<bundle>/ on disk (plugin bundles are never
// embedded), the golden walk's second root.
func pluginAgentBundleDirs(t *testing.T) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join("..", "..", ".agents", "plugins", "*", "agents", "*"))
	if err != nil {
		t.Fatalf("glob plugin agent bundles: %v", err)
	}
	var dirs []string
	for _, m := range matches {
		if st, err := os.Stat(m); err == nil && st.IsDir() {
			dirs = append(dirs, m)
		}
	}
	sort.Strings(dirs)
	return dirs
}

// TestGoldenAgentPrompts pins the assembled system prompt of every shipped and plugin agent bundle.
func TestGoldenAgentPrompts(t *testing.T) {
	des, err := fs.ReadDir(bundledir.SubFS("agents"), ".")
	if err != nil {
		t.Fatalf("list agents: %v", err)
	}
	type bundle struct{ dir, goldenName string }
	var bundles []bundle
	for _, de := range des {
		if de.IsDir() {
			bundles = append(bundles, bundle{path.Join("agents", de.Name()), "agent." + de.Name() + ".txt"})
		}
	}
	for _, dir := range pluginAgentBundleDirs(t) {
		bundles = append(bundles, bundle{dir, "agent." + filepath.Base(dir) + ".txt"})
	}

	for _, bd := range bundles {
		b, err := LoadBundle(context.Background(), nil, bd.dir)
		if err != nil {
			t.Fatalf("%s: %v", bd.dir, err)
		}
		mem, _, err := LoadBundleMemory(context.Background(), nil, bd.dir)
		if err != nil {
			t.Fatalf("%s: %v", bd.dir, err)
		}
		got := promptbuilder.Agent(b.Card.Name, b.Card.Description, nil, false, BehaviourLayer(b.Prompt, mem), "", "")
		checkGolden(t, bd.goldenName, got)
	}
	if len(bundles) == 0 {
		t.Fatal("no agent bundles found")
	}
}

// TestGoldenACPPreamble pins the ACP preamble as serve.buildACPNode assembles it; only this golden covers
// the skills bullet without the load_skill hint and the workspace layer.
func TestGoldenACPPreamble(t *testing.T) {
	const dir = "../../.agents/plugins/github/agents/code-reviewer"
	b, err := LoadBundle(context.Background(), nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	mem, _, err := LoadBundleMemory(context.Background(), nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	skills := []*skill.Frontmatter{{Name: "ponytail-review", Description: "Review for over-engineering."}}
	got := promptbuilder.Agent(b.Card.Name, b.Card.Description, skills, true,
		BehaviourLayer(b.Prompt, mem), promptbuilder.GradingFacts(0.7, 2, true, false),
		"## Workspace\n\nThe repo is cloned at the cwd.")
	checkGolden(t, "acp.preamble.code-reviewer.txt", got)
}

// TestGoldenCompactionPrompt pins the summarizer prompt NativeCompactionConfig builds.
func TestGoldenCompactionPrompt(t *testing.T) {
	sys, tmpl, err := compactionPrompts(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "compaction.txt", sys+"\n\n"+tmpl)
}
