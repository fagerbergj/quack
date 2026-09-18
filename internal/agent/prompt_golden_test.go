package agent

import (
	"context"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"

	"github.com/fagerbergj/quack/internal/bundledir"
	"github.com/fagerbergj/quack/internal/promptbuilder"
)

// updateGolden regenerates testdata/prompts. The goldens were captured before
// the artifact-resolver change (#1420) and must stay byte-identical after it.
var updateGolden = flag.Bool("update-golden", false, "rewrite the prompt golden files")

// todayLine: the Environment layer's only non-deterministic input.
var todayLine = regexp.MustCompile(`Today is \d{4}-\d{2}-\d{2}\.`)

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

// pluginAgentBundleDirs lists every .agents/plugins/<name>/agents/<bundle>/
// directory on disk (a plugin bundle is never embedded, only ever a live
// checkout) - the golden walk's second root, alongside the shipped agents/.
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

// TestGoldenAgentPrompts pins every shipped agent's assembled system prompt,
// plus every plugin's own agent bundles (.agents/plugins/*/agents/*) - a
// plugin bundle's prompt.md is pinned exactly like a shipped one.
func TestGoldenAgentPrompts(t *testing.T) {
	des, err := fs.ReadDir(bundledir.SubFS("agents"), ".")
	if err != nil {
		t.Fatalf("list agents: %v", err)
	}
	type bundle struct{ dir, goldenName string }
	var bundles []bundle
	for _, de := range des {
		if de.IsDir() {
			bundles = append(bundles, bundle{bundledir.PathJoin("agents", de.Name()), "agent." + de.Name() + ".txt"})
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
		got := promptbuilder.Agent(b.Card.Name, b.Card.Description, nil, nil, false, BehaviourLayer(b.Prompt, mem), "", "")
		checkGolden(t, bd.goldenName, got)
	}
	if len(bundles) == 0 {
		t.Fatal("no agent bundles found")
	}
}

// TestGoldenACPPreamble pins the ACP preamble shape - promptbuilder.Agent with
// acp=true, declared skills and a workspace block, as serve.buildACPNode
// assembles it. The skills bullet without the load_skill hint, and the
// workspace layer, are what differ from a native agent's prompt; only this
// golden covers that branch.
func TestGoldenACPPreamble(t *testing.T) {
	const dir = "agents/code-reviewer"
	b, err := LoadBundle(context.Background(), nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	mem, _, err := LoadBundleMemory(context.Background(), nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	skills := []*skill.Frontmatter{{Name: "ponytail-review", Description: "Review for over-engineering."}}
	got := promptbuilder.Agent(b.Card.Name, b.Card.Description, nil, skills, true,
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
