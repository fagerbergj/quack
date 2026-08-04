// A deterministic check against #684: the judge grounds claims in the
// verification report a node produced, not in the repo tree itself - a
// misspelled package path that's faithfully copied from a typo'd report
// scores as "grounded" every time, because every criterion the judge CAN
// evaluate was genuinely satisfied. Same shape as mermaidCriterion (#448):
// detect and report deterministically, never let the model reason about it.
package vetting

import (
	"fmt"
	"io/fs"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/fagerbergj/quack/internal/workspace"
)

// pathBacktickRe matches a single-backtick inline code span - the convention
// this codebase (and most Markdown) uses to mark a literal path/identifier,
// as opposed to a path merely mentioned in prose. Extraction is deliberately
// scoped to marked spans (backticks, Markdown links, GitHub blob URLs): a
// bare, unquoted "app/router/page.tsx" in a sentence about a different
// framework is exactly the false positive this check must never trip on, and
// there is no reliable way to tell "this repo" prose from "some other
// project" prose except by whether the author bothered to mark it as code.
var pathBacktickRe = regexp.MustCompile("`([^`\n]+)`")

// githubBlobRe matches a GitHub blob/tree/raw URL: owner, repo, kind
// (blob/tree - a "tree" link names a directory, never leniently), path.
var githubBlobRe = regexp.MustCompile(`https?://github\.com/([\w.-]+)/([\w.-]+)/(blob|tree|raw)/[^/\s]+/([^\s)\]]+)`)

// lineSuffixRe strips a trailing grep-style ":81" or ":81-92" line reference
// (this codebase's own file:line convention) from a cited path before
// resolution - the file on disk is never actually named "checks.go:81".
var lineSuffixRe = regexp.MustCompile(`:\d+(?:-\d+)?$`)

// pathHit is one extracted candidate: the repo-relative path plus whether it
// was cited AS a directory (a trailing "/", or a GitHub "tree" URL) - see
// pathIndex.resolves for why that distinction is load-bearing.
type pathHit struct {
	rel    string
	dirLik bool
}

// repoPathsResolveCriterion is the GATE side of #684: extracts repo-relative
// paths from the answer (and every staged delivery body - deliveryTexts,
// same set mermaidCriterion scans) and asserts each one resolves against the
// CLONE actually on disk, never against the answer's own citations or the
// verification report it may be quoting - that is the exact blind spot that
// let 39 of 40 misspelled package paths through a judge round that scored
// them "grounded" (#684's evidence).
//
// ok=false, no entry: the node has no clone to check against (a research/
// synthesis run - resolveCiteCloneRoots is empty), or every extracted path
// resolved. A node with a clone but zero extracted paths also returns
// ok=false: silence, not a pass, since there was nothing to check.
func repoPathsResolveCriterion(answer string, act workerActivity, cfg Config) (criterionScore, bool) {
	roots := resolveCiteCloneRoots(cfg, act)
	if len(roots) == 0 {
		return criterionScore{}, false
	}
	idx := buildPathIndex(roots)
	repoIDs := cloneRepoIdentities(roots)

	seen := map[string]bool{}
	var bad []string
	for _, t := range deliveryTexts(answer, act) {
		for _, hit := range extractRepoPaths(t, repoIDs) {
			if seen[hit.rel] {
				continue
			}
			seen[hit.rel] = true
			if !idx.resolves(hit) {
				bad = append(bad, hit.rel)
			}
		}
	}
	if len(bad) == 0 {
		return criterionScore{}, false
	}
	sort.Strings(bad)
	return criterionScore{Score: 0, Reason: fmt.Sprintf(
		"deterministic: %d path(s) do not resolve in the clone: %s - verify each against the repo tree itself, "+
			"not the report or answer that named it", len(bad), strings.Join(bad, ", "))}, true
}

// extractRepoPaths pulls candidate repo-relative paths out of text: backtick
// spans, local (schemeless) Markdown link targets, the code-explorer's
// "repo@path" citation format, and GitHub blob/tree/raw URLs whose owner/repo
// matches repoIDs (a set of "github.com/owner/repo" identities - see
// cloneRepoIdentities). A blob URL for a DIFFERENT repo is never extracted:
// its path has no business resolving against this clone.
func extractRepoPaths(text string, repoIDs map[string]bool) []pathHit {
	var out []pathHit
	for _, m := range pathBacktickRe.FindAllStringSubmatch(text, -1) {
		if hit, ok := candidatePath(m[1]); ok {
			out = append(out, hit)
		}
	}
	for _, m := range markdownLinkRe.FindAllStringSubmatch(text, -1) {
		if hit, ok := candidatePath(m[1]); ok {
			out = append(out, hit)
		}
	}
	for _, m := range codeCiteRe.FindAllStringSubmatch(text, -1) {
		if p := normalizePath(m[2]); p != "" {
			out = append(out, pathHit{rel: p}) // always a file citation - line range or not
		}
	}
	for _, m := range githubBlobRe.FindAllStringSubmatch(text, -1) {
		owner, repo, kind, p := strings.ToLower(m[1]), strings.ToLower(m[2]), m[3], m[4]
		if !repoIDs["github.com/"+owner+"/"+repo] {
			continue
		}
		p, _, _ = strings.Cut(p, "#")
		p, _, _ = strings.Cut(p, "?")
		if np := normalizePath(p); np != "" {
			out = append(out, pathHit{rel: np, dirLik: kind == "tree"})
		}
	}
	return out
}

// candidatePath filters one backtick/link target down to a repo-relative
// path, or ok=false when it plainly isn't one: a web URL or other scheme (a
// blob URL is handled separately, by githubBlobRe, so its path is checked
// against the RIGHT repo), or text with no "/" at all (too weak a signal - a
// bare filename in backticks is as likely to be a shell command or
// identifier as a path). A trailing "/" marks it as a directory citation.
func candidatePath(raw string) (pathHit, bool) {
	target := strings.TrimSpace(raw)
	if target == "" || !strings.Contains(target, "/") {
		return pathHit{}, false
	}
	if u, err := url.Parse(target); err == nil && u.Scheme != "" {
		return pathHit{}, false // a web URL of any scheme - githubBlobRe handles the this-repo case on its own terms
	}
	dirLik := strings.HasSuffix(target, "/")
	target = lineSuffixRe.ReplaceAllString(target, "")
	rel := normalizePath(strings.TrimPrefix(target, "/"))
	if rel == "" {
		return pathHit{}, false
	}
	return pathHit{rel: rel, dirLik: dirLik}, true
}

// pathIndex is every directory and file relpath (forward-slash, relative to
// its clone root) found under one or more clone roots.
type pathIndex struct {
	dirs  map[string]bool
	files map[string]bool
}

// buildPathIndex walks every clone root once, skipping vendored/generated
// trees (workspace.SkipDir - node_modules, build output, dot-dirs) so a
// stale compiled artifact can never stand in for the source tree, nor blow
// up the walk on a large dependency tree.
func buildPathIndex(roots []string) pathIndex {
	idx := pathIndex{dirs: map[string]bool{}, files: map[string]bool{}}
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || p == root {
				return nil
			}
			if d.IsDir() && workspace.SkipDir(d.Name()) {
				return filepath.SkipDir
			}
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil {
				return nil
			}
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				idx.dirs[rel] = true
			} else {
				idx.files[rel] = true
			}
			return nil
		})
	}
	return idx
}

// resolves reports whether hit - a candidate repo-relative path, possibly
// naming a not-yet-created file - is plausible against the tree.
//
//  1. The full path names an existing file or directory (exact, or as a
//     path-bounded SUFFIX of one - "at any depth" - since a citation may be
//     given relative to some subdirectory rather than the clone root).
//  2. Failing that, a FILE-shaped hit (no trailing "/", not a "tree" URL) is
//     still accepted if its immediate parent directory exists: the plan
//     proposes creating it, in a directory that's real.
//
// A directory-shaped hit gets NO such leniency - it must resolve outright.
// That asymmetry is what makes `com/wit/jasonfargerberg/` (#684's exact
// case) still fail: its parent `com/wit` is real, so a file cited under it
// would pass, but the package directory itself never exists under that
// name - forgiving it via one-level "not yet created" tolerance would
// forgive the very typo this check exists to catch.
func (idx pathIndex) resolves(hit pathHit) bool {
	rel := strings.Trim(hit.rel, "/")
	if rel == "" {
		return true
	}
	if pathSetHas(idx.dirs, rel) || pathSetHas(idx.files, rel) {
		return true
	}
	if hit.dirLik {
		return false
	}
	parent := path.Dir(rel)
	if parent == "." {
		return true // top-level - the clone root itself always resolves
	}
	return pathSetHas(idx.dirs, parent)
}

// pathSetHas reports whether rel is exactly in set, or is a path-component-
// bounded suffix of some entry in it (an entry "a/b/c" backs a rel of "b/c"
// but never "wrong-b/c" or "xb/c" - the boundary is what keeps this from
// matching an unrelated directory that merely ends with the same characters).
func pathSetHas(set map[string]bool, rel string) bool {
	if set[rel] {
		return true
	}
	suffix := "/" + rel
	for entry := range set {
		if strings.HasSuffix(entry, suffix) {
			return true
		}
	}
	return false
}

// cloneRepoIdentities is the set of "github.com/owner/repo" identities
// (RepoIdentity, lowercased) that ARE this node's clone(s) - the this-repo
// filter for githubBlobRe. A clone with no origin remote (a scratch dir with
// no .git, or one never pushed) simply contributes nothing, so a blob URL
// can never match it - the check just finds no GitHub-URL paths to verify
// there, same graceful-degradation posture as an unresolvable clone root.
func cloneRepoIdentities(roots []string) map[string]bool {
	out := map[string]bool{}
	for _, root := range roots {
		if id := workspace.RepoIdentity(root); id != "" {
			out[strings.ToLower(id)] = true
		}
	}
	return out
}
