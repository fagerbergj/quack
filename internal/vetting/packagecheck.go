// A deterministic gate check for #684: a JVM/Go package/directory mismatch
// caught by ANCHORING TO THE LANGUAGE GRAMMAR, not by scanning prose for
// slash-shaped strings. #685 tried the latter (backtick spans containing a
// slash) and was reverted (#695) - it flagged Android resource references
// (`@color/x`) and Kotlin method chains, because "text with a slash" is not
// decidable as a path from string shape alone. A `package` declaration IS
// decidable: it's a specific AST node, and Kotlin/Java require it to
// correspond to the file's own directory (Go: to the directory's base name).
// ast-grep finds the declaration structurally, so nothing that isn't one can
// ever match.
package vetting

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/fagerbergj/quack/internal/workspace"
)

// probeAstGrepPackages names this probe's execute_tool ledger event
// (emitProbeEvent, probeemit.go) - one per language ast-grep scans.
const probeAstGrepPackages = "ast_grep_package_declarations"

// astGrepPackagePattern matches a package declaration in Kotlin, Java, or
// Go's grammar - each language's `-l` flag compiles it against that
// language's own tree-sitter grammar, so it can only ever bind to the real
// package-declaration node, never to an unrelated token that happens to
// contain the word "package" or a slash.
const astGrepPackagePattern = "package $PKG"

// astGrepPackageLangs are the ast-grep `--lang` values this check scans;
// each also implicitly restricts which file extensions ast-grep visits
// (.kt/.java/.go respectively) - see hasCandidateSource for the mirror list.
var astGrepPackageLangs = []string{"kotlin", "java", "go"}

// packageCheckExts is hasCandidateSource's cheap pre-flight: whether ANY
// file worth scanning exists, so a repo with none of these languages never
// invokes ast-grep (or warns about its absence) at all.
var packageCheckExts = map[string]bool{".kt": true, ".java": true, ".go": true}

// astGrepMatch is one `ast-grep run --json=compact` match, trimmed to the
// fields this check reads.
type astGrepMatch struct {
	File          string `json:"file"`
	MetaVariables struct {
		Single map[string]struct {
			Text string `json:"text"`
		} `json:"single"`
	} `json:"metaVariables"`
}

// packageMatch is one extracted package declaration: which language pattern
// found it, the file it's in (absolute), and the declared package text.
type packageMatch struct {
	lang string
	file string
	pkg  string
}

var warnAstGrepUnavailable = sync.OnceFunc(func() {
	slog.Warn("package-declaration check is not running: ast-grep was not found on PATH", "component", "vetting")
})

// packageDeclarationCriterion is the GATE side of #684: every Kotlin/Java/Go
// package declaration in the node's repo must match its own directory (see
// checkGoPackage/checkJVMPackage). Reuses checksDir/checksCaps - the SAME
// repo location and sandbox checksPassCriterion runs build/vet/test in - so
// this never scans a directory the node's own checks didn't.
//
// ok=false (no criterion entry, not a pass): no repo to scan, nothing
// resembling Kotlin/Java/Go in it, ast-grep missing (logged once, loud - see
// warnAstGrepUnavailable), the scan itself errored (logged, not the node's
// fault), or every declaration matched. This check is independent of
// cfg.DeriveChecks/cfg.Checks - a plan that set no build checks still gets
// it - but is still bound to the SAME operator allowlist those go through
// (workspace.check_commands): an operator who never lists "ast-grep" there
// gets no invocation of it, ever.
func packageDeclarationCriterion(ctx context.Context, cfg Config) (criterionScore, bool) {
	if cfg.Workspace == nil || !workspace.MatchesCheckPrefix("ast-grep", cfg.CheckCommands) {
		return criterionScore{}, false
	}
	dir, ok, err := checksDir(cfg)
	if err != nil || !ok {
		return criterionScore{}, false
	}
	if !hasCandidateSource(dir) {
		return criterionScore{}, false
	}
	if _, err := workspace.ResolveExecutable("", "ast-grep"); err != nil {
		warnAstGrepUnavailable()
		return criterionScore{}, false
	}
	matches, err := scanPackageDeclarations(ctx, dir, checksCaps(cfg))
	if err != nil {
		slog.Warn("ast-grep package-declaration scan failed; skipping", "component", "vetting", "node", cfg.NodeID, "err", err)
		return criterionScore{}, false
	}
	var bad []string
	for _, m := range matches {
		if f := checkPackageMatch(m, dir); f != "" {
			bad = append(bad, f)
		}
	}
	if len(bad) == 0 {
		return criterionScore{}, false
	}
	sort.Strings(bad)
	return criterionScore{Score: 0, Reason: fmt.Sprintf(
		"deterministic: %d file(s) declare a package that does not match their directory:\n%s",
		len(bad), strings.Join(bad, "\n"))}, true
}

// hasCandidateSource reports whether dir contains any file this check would
// scan, skipping vendored/generated trees (workspace.SkipDir) exactly like
// FindRepos - a compiled/vendored .go|.kt|.java under node_modules or a
// build/ output dir is not code this run wrote and must never be scanned.
func hasCandidateSource(dir string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != dir && workspace.SkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if packageCheckExts[filepath.Ext(d.Name())] {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// scanPackageDeclarations runs one `ast-grep run` per language over dir,
// through the SAME argv-only runner and sandbox caps every other derived
// check uses (workspace.RunArgv - never a shell), with SkipGlobs excluding
// vendored/generated trees.
//
// ast-grep's `run` follows grep's OWN exit convention (0 = matches, 1 = none
// - not an error), so only a HIGHER code or a launch failure is treated as a
// scan error here; "found nothing" is the normal, silent case.
func scanPackageDeclarations(ctx context.Context, dir string, caps workspace.Caps) ([]packageMatch, error) {
	var all []packageMatch
	for _, lang := range astGrepPackageLangs {
		argv := []string{"ast-grep", "run", "-p", astGrepPackagePattern, "-l", lang, "--json=compact"}
		for _, g := range workspace.SkipGlobs() {
			argv = append(argv, "--globs", g)
		}
		argv = append(argv, dir)
		res, runErr := workspace.RunArgv(ctx, dir, argv, caps)
		var probeResult map[string]any
		if runErr == nil {
			probeResult = map[string]any{"exit_code": res.ExitCode}
		}
		emitProbeEvent(ctx, probeAstGrepPackages, map[string]any{"lang": lang}, probeResult, runErr)
		if runErr != nil {
			return nil, fmt.Errorf("ast-grep (%s): %w", lang, runErr)
		}
		if res.ExitCode > 1 {
			return nil, fmt.Errorf("ast-grep (%s) exited %d: %s", lang, res.ExitCode, res.Output)
		}
		out := strings.TrimSpace(res.Output)
		if out == "" {
			continue
		}
		var matches []astGrepMatch
		if err := json.Unmarshal([]byte(out), &matches); err != nil {
			return nil, fmt.Errorf("ast-grep (%s) output: %w", lang, err)
		}
		for _, m := range matches {
			pkg := m.MetaVariables.Single["PKG"].Text
			if pkg == "" || m.File == "" {
				continue
			}
			all = append(all, packageMatch{lang: lang, file: m.File, pkg: pkg})
		}
	}
	return all, nil
}

// checkPackageMatch reports "" when m's declared package matches its file's
// directory, else a message naming the file, the declared package, and what
// directory would satisfy it - so the model reading it as revise feedback
// can fix either side without guessing.
func checkPackageMatch(m packageMatch, repoDir string) string {
	relFile, err := filepath.Rel(repoDir, m.file)
	if err != nil {
		return ""
	}
	relFile = filepath.ToSlash(relFile)
	relDir := path.Dir(relFile)

	var want string
	if m.lang == "go" {
		want = checkGoPackage(relFile, relDir, m.pkg, repoDir)
	} else {
		want = checkJVMPackage(relDir, m.pkg)
	}
	if want == "" {
		return ""
	}
	return fmt.Sprintf("%s: declares package %q, but its directory is %q - want %s", relFile, m.pkg, relDir, want)
}

// checkJVMPackage implements Kotlin/Java's convention: the package's
// dot-segments must be a trailing, path-component-bounded SUFFIX of the
// file's directory - not the whole directory, since a source root prefix
// (src/main/java, src/main/kotlin, a Gradle module name, …) legitimately
// precedes it and this check has no way to know which prefix a given repo
// uses. Returns "" (match) or a description of the suffix that was wanted.
func checkJVMPackage(relDir, pkg string) string {
	pkgSegs := strings.Split(pkg, ".")
	var dirSegs []string
	if relDir != "." {
		dirSegs = strings.Split(relDir, "/")
	}
	if len(dirSegs) >= len(pkgSegs) {
		tail := dirSegs[len(dirSegs)-len(pkgSegs):]
		if strings.Join(tail, "/") == strings.Join(pkgSegs, "/") {
			return ""
		}
	}
	return fmt.Sprintf("a directory ending %q", strings.Join(pkgSegs, "/"))
}

// checkGoPackage implements Go's convention: the package name equals the
// file's own containing directory's base name (repoDir's own base name for a
// file at the repo root) - a single component, unlike JVM's dotted
// hierarchy, since Go packages are not nested by name. "main" is exempt (any
// directory may hold a command), and a "_test.go" file may additionally
// declare the directory's name plus "_test" (an external test package).
func checkGoPackage(relFile, relDir, pkg, repoDir string) string {
	if pkg == "main" {
		return ""
	}
	dirName := path.Base(relDir)
	if relDir == "." {
		dirName = filepath.Base(repoDir)
	}
	if pkg == dirName {
		return ""
	}
	if strings.HasSuffix(relFile, "_test.go") && pkg == dirName+"_test" {
		return ""
	}
	return fmt.Sprintf("directory %q (or %q for an external test file)", dirName, dirName+"_test")
}
