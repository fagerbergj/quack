package serve

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

var loadSkillRe = regexp.MustCompile(`load_skill\("([a-zA-Z0-9_-]+)"\)`)

// Every skill an agent's prompt tells it to load MUST exist in the skill
// library we actually ship (embedded skills/ plus vendored ponytail skills).
// A prompt naming an unshipped skill is not a harmless typo: the agent's
// FIRST action fails, and it flails.
//
// Regression: agents/code-explorer/prompt.md loaded a skill that only lived
// in .agents/skills/ (project skills, loadable only after cd'ing into the
// quack repo), never the shipped library - so every explorer run began by
// failing its own mandatory discipline.
func TestEveryAgentPromptSkillIsShipped(t *testing.T) {
	root := repoRoot(t)

	// The vendored ponytail skills are a git SUBMODULE (see .gitmodules and
	// serve.go's vendorSkillsDir). CI and the Docker build initialise it; a plain
	// `git worktree add` does not. Without it we cannot tell "the prompt names a
	// skill we never ship" (a real bug) from "this checkout just hasn't inited the
	// submodule" (a local artifact) - so skip rather than cry wolf at every dev.
	vendor := filepath.Join(root, ".agents", "vendor", "ponytail", "skills")
	if st, err := os.Stat(vendor); err != nil || !st.IsDir() {
		t.Skip("vendored skills submodule not initialised (git submodule update --init); cannot verify vendored skill references")
	}

	// dotagents needs no such skip: its embed (embed.go) already fails the
	// build when the submodule is missing, so reaching this test implies it.
	shipped := map[string]bool{}
	for _, dir := range []string{filepath.Join(root, "skills"), filepath.Join(root, dotagentsSkillsDir), vendor} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				shipped[e.Name()] = true
			}
		}
	}
	if len(shipped) == 0 {
		t.Fatal("found no shipped skills at all; the test cannot be meaningful")
	}

	prompts, err := filepath.Glob(filepath.Join(root, "agents", "*", "prompt.md"))
	if err != nil || len(prompts) == 0 {
		t.Fatalf("no agent prompts found: %v", err)
	}

	for _, p := range prompts {
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		agent := filepath.Base(filepath.Dir(p))
		for _, m := range loadSkillRe.FindAllStringSubmatch(string(body), -1) {
			name := m[1]
			if !shipped[name] {
				t.Errorf("agent %q tells itself to load_skill(%q), but we do not ship that skill.\n"+
					"Its first action will fail and it will flail. Ship the skill in skills/ (or vendor it), "+
					"or remove the instruction from the prompt.", agent, name)
			}
		}
	}
}

// repoRoot walks up from the test's working directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not locate the repo root (go.mod)")
	return ""
}
