package serve

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/fagerbergj/quack/internal/plugin"
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

	// Mirror what actually reaches an agent (internal/serve's newSkillSource):
	// quack's own skills/, plus plugin-resolved roots, plus dotagents' skills/
	// directly - dotagents ships no plugin manifest, so it reaches the runtime
	// via the embedded fallback (dotagentsEmbeddedSkills), not discovery.
	//
	// The trees are in-tree, so an unresolvable root is a real breakage. This
	// used to t.Skip unless BOTH roots resolved, which never happened even with
	// submodules initialised - so this check has never actually run.
	ponytail := filepath.Join(root, ".agents", "vendor", "ponytail")
	vendorDirs := plugin.ResolveSkillDirs([]string{ponytail})
	if len(vendorDirs) != 1 {
		t.Fatalf("vendored ponytail plugin did not resolve at %s", ponytail)
	}
	vendorDirs = append(vendorDirs, filepath.Join(root, dotagentsEmbeddedSkills))

	shipped := map[string]bool{}
	for _, dir := range append([]string{filepath.Join(root, "skills")}, vendorDirs...) {
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
