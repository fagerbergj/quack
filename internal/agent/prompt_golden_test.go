package agent

import (
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"testing"

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

// TestGoldenAgentPrompts pins every shipped agent's assembled system prompt.
func TestGoldenAgentPrompts(t *testing.T) {
	des, err := fs.ReadDir(bundledir.SubFS("agents"), ".")
	if err != nil {
		t.Fatalf("list agents: %v", err)
	}
	seen := 0
	for _, de := range des {
		if !de.IsDir() {
			continue
		}
		dir := bundledir.PathJoin("agents", de.Name())
		b, err := LoadBundle(dir)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		mem, err := LoadBundleMemory(dir)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		got := promptbuilder.Agent(b.Card.Name, b.Card.Description, nil, nil, false, behaviourLayer(b.Prompt, mem), "", "")
		checkGolden(t, "agent."+de.Name()+".txt", got)
		seen++
	}
	if seen == 0 {
		t.Fatal("no agent bundles found")
	}
}

// TestGoldenCompactionPrompt pins the summarizer prompt NativeCompactionConfig builds.
func TestGoldenCompactionPrompt(t *testing.T) {
	sys, tmpl, err := compactionPrompts()
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "compaction.txt", sys+"\n\n"+tmpl)
}
