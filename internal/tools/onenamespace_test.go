package tools

import (
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/workspace"
)

// The node dir is an INVISIBLE ROOT: the model must never see it in any tool's
// arguments or results. There is exactly ONE namespace — paths relative to the
// node's own root (and, within it, relative to the cwd).
//
// The live bug this pins: `cd` reported its new dir CHAT-relative (carrying the
// node dir, "explorer-openhands/openhands") while git_clone and list_dir reported
// the same location CWD-relative ("openhands"). The model was handed two
// incompatible namespaces, faithfully reused `cd`'s path, and flailed — one
// explorer node made 34 REPEATED calls out of 69 doing exactly this.
//
// The assertion is the exact live sequence: cd → take its reported `dir` → feed
// it straight back to read_file/list_dir/grep.
func TestCdReportedDirCarriesNoNodePrefixAndFeedsBack(t *testing.T) {
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := fsBinding{userID: "u1", jail: j, caps: workspace.DefaultCaps()}
	ctx := newGatedCtx(t, "plan-1", "explorer-openhands", "chat-1")

	// A repo under the node's own dir (where git_clone lands it), written with the
	// same cwd-relative paths the worker's tools speak.
	if _, err := b.withCwd(ctx).writeFile(writeFileArgs{Path: "openhands/README.md", Content: "# OpenHands"}); err != nil {
		t.Fatalf("write_file: %v", err)
	}

	res, err := b.withCwd(ctx).cd(ctx, cdArgs{Dir: "openhands"})
	if err != nil {
		t.Fatalf("cd openhands: %v", err)
	}
	if strings.Contains(res.Dir, "explorer-openhands") {
		t.Fatalf("cd reported dir %q — it LEAKS the node dir; the node dir must be invisible", res.Dir)
	}
	if res.Dir != "openhands" {
		t.Fatalf("cd reported dir %q, want %q (node-relative, matching what git_clone/list_dir report)", res.Dir, "openhands")
	}

	// Feed cd's OWN reported path straight back — this is what the model does.
	if got, err := b.withCwd(ctx).readFile(readFileArgs{Path: res.Dir + "/README.md"}); err != nil {
		t.Errorf("read_file(%q) after cd: %v", res.Dir+"/README.md", err)
	} else if !strings.Contains(got.Content, "OpenHands") {
		t.Errorf("read_file returned %q", got.Content)
	}
	if _, err := b.withCwd(ctx).listDir(listDirArgs{Path: res.Dir}); err != nil {
		t.Errorf("list_dir(%q) after cd: %v", res.Dir, err)
	}
	if _, err := b.withCwd(ctx).grep(grepArgs{Pattern: "OpenHands", Path: res.Dir}); err != nil {
		t.Errorf("grep in %q after cd: %v", res.Dir, err)
	}

	// A BARE relative path still resolves against the cwd (the shell contract).
	if got, err := b.withCwd(ctx).readFile(readFileArgs{Path: "README.md"}); err != nil {
		t.Errorf("bare relative read after cd: %v", err)
	} else if !strings.Contains(got.Content, "OpenHands") {
		t.Errorf("bare relative read returned %q", got.Content)
	}
}

// `cd`'s project context (nearest AGENTS.md + project skills) must still find the
// repo BELOW the node dir — and report the instructions path in the one namespace,
// so the model can read it back.
func TestCdProjectContextFoundBelowTheNodeDir(t *testing.T) {
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := fsBinding{userID: "u1", jail: j, caps: workspace.DefaultCaps()}
	ctx := newGatedCtx(t, "plan-1", "impl-node", "chat-1")

	for path, content := range map[string]string{
		"repo/AGENTS.md":                     "run make test",
		"repo/.agents/skills/alpha/SKILL.md": skillFile("alpha", "the alpha skill"),
		"repo/main.go":                       "package main",
	} {
		if _, err := b.withCwd(ctx).writeFile(writeFileArgs{Path: path, Content: content}); err != nil {
			t.Fatalf("write_file %s: %v", path, err)
		}
	}

	res, err := b.withCwd(ctx).cd(ctx, cdArgs{Dir: "repo"})
	if err != nil {
		t.Fatalf("cd repo: %v", err)
	}
	if res.Instructions != "run make test" {
		t.Errorf("Instructions = %q, want the repo's AGENTS.md below the node dir", res.Instructions)
	}
	if res.InstructionsPath != "repo/AGENTS.md" {
		t.Errorf("InstructionsPath = %q, want %q (node-relative — no node-dir prefix)", res.InstructionsPath, "repo/AGENTS.md")
	}
	// The reported instructions path must itself be readable back.
	if _, err := b.withCwd(ctx).readFile(readFileArgs{Path: res.InstructionsPath}); err != nil {
		t.Errorf("read_file(%q) — cd's own reported instructions path: %v", res.InstructionsPath, err)
	}
	if len(res.Skills) != 1 || res.Skills[0].Name != "alpha" {
		t.Errorf("skills = %+v, want the repo's alpha skill discovered below the node dir", res.Skills)
	}
}

// No tool result ever names the node dir: git_clone, list_dir and cd must all
// describe the SAME location with the SAME string.
func TestCloneListAndCdAgreeOnOneNamespace(t *testing.T) {
	requireGit(t)
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	gb := gitBinding{userID: "u1", jail: j, caps: workspace.DefaultCaps()}
	fb := fsBinding{userID: "u1", jail: j, caps: workspace.DefaultCaps()}
	ctx := newGatedCtx(t, "plan-1", "explorer-openhands", "chat-1")

	cloned, err := gb.withCwd(ctx).cloneRepo("file://"+newBareRepoFixture(t), "openhands", nil, "")
	if err != nil {
		t.Fatalf("git_clone: %v", err)
	}
	if cloned.Dir != "openhands" {
		t.Fatalf("git_clone reported dir %q, want %q", cloned.Dir, "openhands")
	}
	cd, err := fb.withCwd(ctx).cd(ctx, cdArgs{Dir: cloned.Dir})
	if err != nil {
		t.Fatalf("cd %q (git_clone's own reported dir): %v", cloned.Dir, err)
	}
	if cd.Dir != cloned.Dir {
		t.Fatalf("cd reported %q but git_clone reported %q for the same dir — two namespaces", cd.Dir, cloned.Dir)
	}
}

// `cd` must prove it moved: report where you now stand, and what is there.
//
// The live failure (code-mode dogfood, 2026-07-13): cd answered with the same string it
// was handed — cd("goose") -> {"dir": "goose"} — which is indistinguishable from a no-op.
// The model had no evidence it had moved, so it re-cd'd to the SAME directory, and
// elsewhere ran list_dir on the directory it was already standing in. Repeatedly:
// `cd goose` TWICE in one node, `list_dir "."` THREE times in another. Pure wasted turns.
func TestCdProvesItMoved(t *testing.T) {
	j, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b := fsBinding{userID: "u1", jail: j, caps: workspace.DefaultCaps()}
	ctx := newGatedCtx(t, "plan-1", "explorer-goose", "chat-1")

	for _, p := range []string{"goose/Cargo.toml", "goose/README.md"} {
		if _, err := b.withCwd(ctx).writeFile(writeFileArgs{Path: p, Content: "x"}); err != nil {
			t.Fatalf("write_file %s: %v", p, err)
		}
	}

	res, err := b.withCwd(ctx).cd(ctx, cdArgs{Dir: "goose"})
	if err != nil {
		t.Fatalf("cd: %v", err)
	}
	if res.Cwd != "/goose" {
		t.Errorf("cd reported cwd %q, want \"/goose\" — with only `dir` echoing the argument back, the model cannot tell a real cd from a no-op, and cds again", res.Cwd)
	}
	if len(res.Entries) == 0 {
		t.Fatal("cd returned no listing of where it landed — the model must spend a list_dir to confirm arrival, which is exactly what it did live")
	}
	var names []string
	for _, e := range res.Entries {
		names = append(names, e.Path)
	}
	for _, want := range []string{"Cargo.toml", "README.md"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Errorf("cd's listing is missing %q; got %v — it must show what is HERE, in the cwd-relative namespace", want, names)
		}
	}
}
