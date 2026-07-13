package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/skillsource"
)

// projectInstructionFiles are the project agent-instruction filenames, in
// PRECEDENCE order per the agents.md standard (https://agents.md/): the CLOSEST
// AGENTS.md to the working dir wins, and CLAUDE.md is consulted ONLY when no
// AGENTS.md exists anywhere up the chain. No merging — one file wins.
var projectInstructionFiles = []string{"AGENTS.md", "CLAUDE.md"}

type cdArgs struct {
	Dir string `json:"dir"`
}

type cdSkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

type cdResult struct {
	// Dir is the new working directory (workspace-relative, "." = jail root).
	// Every later relative path/dir you pass to a workspace tool resolves
	// against this until the next `cd`.
	Dir string `json:"dir"`
	// InstructionsPath is the workspace-relative path of the AGENTS.md/CLAUDE.md
	// whose content Instructions carries (empty when none was found).
	InstructionsPath string `json:"instructions_path,omitempty"`
	// Instructions is the content of the nearest project agent-instruction file
	// (bounded to the read cap; InstructionsTruncated flags a longer file).
	Instructions          string    `json:"instructions,omitempty"`
	InstructionsTruncated bool      `json:"instructions_truncated,omitempty"`
	Skills                []cdSkill `json:"skills"`
	// Note carries a human-readable summary of what was (not) found — e.g. that
	// no project instructions exist here.
	Note string `json:"note"`
}

// cdDescription tells the agent exactly what `cd` does: it BOTH moves the
// working directory AND reports the repo's own agent instructions + skills.
const cdDescription = "Change your working directory to `dir` (workspace-relative) AND load that location's project " +
	"context. After `cd`, every relative path or `dir` you pass to a workspace tool (read_file, write_file, " +
	"list_dir, grep, git_*, run_command, …) resolves against this directory — so you can pass `README.md` " +
	"instead of `myrepo/README.md`. Prefix a path with `/` to address the workspace root explicitly (ignoring " +
	"the working directory). `cd` returns: the nearest project agent-instructions (the closest AGENTS.md walking " +
	"up to the repo/workspace root — falling back to CLAUDE.md if there is none — which you MUST then follow for " +
	"build/test/style/PR conventions), and the project-level skills that repo defines (loadable with load_skill). " +
	"Run `cd <repo>` right after git_clone."

func newCd(d Deps) (tool.Tool, error) {
	b, err := newFSBinding(d)
	if err != nil {
		return nil, err
	}
	return functiontool.New[cdArgs, cdResult](
		functiontool.Config{Name: "cd", Description: cdDescription},
		func(ctx agent.Context, a cdArgs) (cdResult, error) { return b.withCwd(ctx).cd(ctx, a) },
	)
}

// cd sets the session working directory (CwdKey in ctx state) to `dir` —
// resolved against the CURRENT cwd, so `cd a` then `cd b` lands in a/b, exactly
// like a shell — then reports that location's project context (nearest
// AGENTS.md/CLAUDE.md + discovered project skills). The receiver's b.cwd is the
// CURRENT working directory (set by withCwd); the resolution and the stored new
// cwd are both jail-relative to the workspace root.
func (b fsBinding) cd(ctx agent.Context, a cdArgs) (cdResult, error) {
	realDir, err := b.resolve(a.Dir) // resolves a.Dir against the current cwd, jail-checked
	if err != nil {
		return cdResult{}, err
	}
	info, err := os.Stat(realDir)
	if err != nil {
		return cdResult{}, fmt.Errorf("cd: %w", err)
	}
	if !info.IsDir() {
		return cdResult{}, fmt.Errorf("cd: %q is not a directory", a.Dir)
	}
	chatRoot, err := b.jail.Resolve(b.userID, b.chatID, "")
	if err != nil {
		return cdResult{}, err
	}
	// New cwd, stored as a chat-root-relative slash path ("" = root, persisted
	// as ".") — cwd composes WITHIN the per-chat dir.
	newCwd, err := filepath.Rel(chatRoot, realDir)
	if err != nil {
		return cdResult{}, fmt.Errorf("cd: %w", err)
	}
	newCwd = filepath.ToSlash(newCwd)
	// Stored verbatim, including "." for the chat root: a stored cwd is always
	// NON-empty, which is what distinguishes "the worker deliberately cd'd to the
	// chat root" from "the worker has not cd'd at all" (whose default is the
	// node's own dir — see cwdOrDefault). joinCwd(".", p) == p, so the root case
	// still resolves exactly as before.
	if st := ctx.State(); st != nil {
		if err := st.Set(CwdKey, newCwd); err != nil {
			return cdResult{}, fmt.Errorf("cd: persist working directory: %w", err)
		}
	}

	res := cdResult{Dir: newCwd, Skills: []cdSkill{}}

	// Nearest project instructions: walk UP from realDir to chatRoot (inclusive),
	// AGENTS.md first (closest wins), then CLAUDE.md only if no AGENTS.md exists
	// anywhere in the chain — the agents.md precedence, no merge.
	if path, rel, found := b.nearestInstructions(realDir, chatRoot); found {
		content, truncated, rerr := b.readCappedFile(path)
		if rerr != nil {
			return cdResult{}, fmt.Errorf("cd: read %s: %w", rel, rerr)
		}
		res.InstructionsPath = rel
		res.Instructions = content
		res.InstructionsTruncated = truncated
	}

	// Project skills: the containing repo is the first path component BELOW the
	// node's own dir (where git_clone lands a repo) — or below the chat root when
	// there is no node scope.
	for _, fm := range skillsource.ProjectSkills(b.jail, b.userID, b.chatID, repoRel(newCwd, b.nodeDir)) {
		res.Skills = append(res.Skills, cdSkill{Name: fm.Name, Description: fm.Description})
	}

	res.Note = cdNote(res)
	return res, nil
}

// nearestInstructions walks from startDir up to stopDir (inclusive), returning
// the closest AGENTS.md; if none exists anywhere in the chain, the closest
// CLAUDE.md. rel is the returned file's path relative to the jail root (for the
// result + as a citable source). Both paths are already jail-contained (Resolve
// produced startDir/stopDir), so the walk never reads outside the jail.
func (b fsBinding) nearestInstructions(startDir, stopDir string) (path, rel string, found bool) {
	root, _ := b.jail.Resolve(b.userID, b.chatID, "")
	for _, name := range projectInstructionFiles {
		if p, ok := nearestUp(startDir, stopDir, name); ok {
			r, err := filepath.Rel(root, p)
			if err != nil {
				r = name
			}
			return p, filepath.ToSlash(r), true
		}
	}
	return "", "", false
}

// nearestUp walks from startDir up to stopDir (inclusive) looking for a regular
// file named filename, returning the first (closest) hit. It never climbs above
// stopDir — the jail-root guard.
func nearestUp(startDir, stopDir, filename string) (string, bool) {
	cur := startDir
	for {
		p := filepath.Join(cur, filename)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, true
		}
		if cur == stopDir {
			return "", false
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", false // filesystem root reached (defensive; stopDir should catch first)
		}
		cur = parent
	}
}

// readCappedFile reads an absolute path up to caps.MaxReadBytes, reporting
// whether it was truncated (loudly, via the flag — never a silent short read).
func (b fsBinding) readCappedFile(path string) (content string, truncated bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", false, err
	}
	defer f.Close()
	data, capped, err := readCapped(f, b.caps.MaxReadBytes)
	if err != nil {
		return "", false, err
	}
	return string(data), capped, nil
}

// repoRel returns the chat-root-relative directory of the repo the cwd sits in:
// the first path segment BELOW the node's own dir (git_clone lands a repo there),
// or the first segment of cwd when there is no node scope. "" when the cwd is the
// node dir / chat root itself (no containing repo).
func repoRel(cwd, nodeDir string) string {
	if nodeDir == "" {
		return firstComponent(cwd)
	}
	if cwd != nodeDir && !strings.HasPrefix(cwd, nodeDir+"/") {
		return firstComponent(cwd) // the worker cd'd out of its node dir
	}
	sub := firstComponent(strings.TrimPrefix(strings.TrimPrefix(cwd, nodeDir), "/"))
	if sub == "" {
		return ""
	}
	return nodeDir + "/" + sub
}

// firstComponent returns the first path segment of a slash path ("" for the
// root "." or ""), i.e. the immediate-child repo directory the cwd sits in.
func firstComponent(rel string) string {
	if rel == "" || rel == "." {
		return ""
	}
	if i := strings.IndexByte(rel, '/'); i >= 0 {
		return rel[:i]
	}
	return rel
}

func cdNote(r cdResult) string {
	var parts []string
	if r.InstructionsPath != "" {
		parts = append(parts, fmt.Sprintf("project instructions: %s", r.InstructionsPath))
	} else {
		parts = append(parts, "no project instructions found (no AGENTS.md or CLAUDE.md up to the workspace root)")
	}
	switch n := len(r.Skills); n {
	case 0:
		parts = append(parts, "no project skills")
	case 1:
		parts = append(parts, "1 project skill")
	default:
		parts = append(parts, fmt.Sprintf("%d project skills", n))
	}
	return strings.Join(parts, "; ")
}
