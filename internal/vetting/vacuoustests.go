package vetting

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/fagerbergj/quack/internal/workspace"
)

// A test file that never names a single production identifier can only be
// asserting against locals/lambdas it declares itself - vacuous by
// construction (#716). The oracle is structural (does the test file's text
// contain a token declared in the repo's own non-test source), never a
// re-run of the worker's own claims, so it can't be gamed the way a
// worker-chosen mutation could.
//
// Flaky-gate rule: prefer false negatives. A file whose language we don't
// parse, that isn't unambiguously a test (no @Test/func Test.../etc.), or
// where the repo/diff can't be resolved is SKIPPED, never failed.

// vacuousTestLang: one language's test-file convention, "is this really an
// executed test" signal, and the regex that finds identifiers declared in
// production (non-test) source.
type vacuousTestLang struct {
	name        string
	ext         []string
	isTestFile  func(base string) bool
	hasTestDecl *regexp.Regexp
	declPattern *regexp.Regexp
}

var vacuousTestLangs = []vacuousTestLang{
	{
		name:        "go",
		ext:         []string{".go"},
		isTestFile:  func(base string) bool { return strings.HasSuffix(base, "_test.go") },
		hasTestDecl: regexp.MustCompile(`(?m)^\s*func\s+(?:Test|Example)\w*\s*\(`),
		declPattern: regexp.MustCompile(`(?m)^\s*func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)\s*\(|^\s*type\s+([A-Za-z_]\w*)\s|^\s*(?:var|const)\s+([A-Za-z_]\w*)\b`),
	},
	{
		// Kotlin test convention (JUnit/Compose): FooTest.kt / FooTests.kt with @Test methods.
		name: "kotlin",
		ext:  []string{".kt", ".kts"},
		isTestFile: func(base string) bool {
			return strings.HasSuffix(base, "Test.kt") || strings.HasSuffix(base, "Tests.kt")
		},
		hasTestDecl: regexp.MustCompile(`@Test\b`),
		declPattern: regexp.MustCompile(`\b(?:class|interface|object)\s+([A-Za-z_]\w*)|\bfun\s+([A-Za-z_]\w*)\s*\(|\b(?:val|var)\s+([A-Za-z_]\w*)\s*[:=]`),
	},
	{
		name: "java",
		ext:  []string{".java"},
		isTestFile: func(base string) bool {
			return strings.HasSuffix(base, "Test.java") || strings.HasSuffix(base, "Tests.java")
		},
		hasTestDecl: regexp.MustCompile(`@Test\b`),
		// Method-signature extraction for Java is too ambiguous to do reliably (control-flow
		// keywords look like calls); class/interface/enum names only - narrower, never flaky.
		declPattern: regexp.MustCompile(`\b(?:class|interface|enum)\s+([A-Za-z_]\w*)`),
	},
	{
		name:        "js-ts",
		ext:         []string{".ts", ".tsx", ".js", ".jsx"},
		isTestFile:  func(base string) bool { return strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") },
		hasTestDecl: regexp.MustCompile("\\b(it|test)\\s*\\(\\s*['\"`]"),
		declPattern: regexp.MustCompile(`\bfunction\s+([A-Za-z_]\w*)|\bclass\s+([A-Za-z_]\w*)|\binterface\s+([A-Za-z_]\w*)|\btype\s+([A-Za-z_]\w*)\s*=|\bconst\s+([A-Za-z_]\w*)\s*=`),
	},
	{
		name:        "python",
		ext:         []string{".py"},
		isTestFile:  func(base string) bool { return strings.HasPrefix(base, "test_") || strings.HasSuffix(base, "_test.py") },
		hasTestDecl: regexp.MustCompile(`(?m)^\s*def\s+test_\w*\s*\(`),
		declPattern: regexp.MustCompile(`(?m)^\s*def\s+([A-Za-z_]\w*)\s*\(|^\s*class\s+([A-Za-z_]\w*)\b`),
	},
}

// vacuousTestSkipDirs never hold production source worth walking - deps/build output.
var vacuousTestSkipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "dist": true, "build": true,
	"target": true, "out": true, ".gradle": true, ".idea": true,
}

// identTokenRe tokenizes a test file's raw text into candidate identifiers.
var identTokenRe = regexp.MustCompile(`[A-Za-z_]\w*`)

// vacuousTestsCriterion: added test files that reference no production identifier
// are vacuous by construction. Runs only against THIS node's own new commits
// (base = cfg.NodeBaseSHA, same scoping as checksPassCriterion/#710) and only
// against files git reports as newly ADDED - a pre-existing test edited in
// place is out of scope, see issue #716.
func vacuousTestsCriterion(cfg Config) (criterionScore, bool) {
	if cfg.Workspace == nil || cfg.ReadOnly {
		return criterionScore{}, false
	}
	dir, ok, err := checksDir(cfg)
	if err != nil || !ok {
		return criterionScore{}, false
	}
	caps := checksCaps(cfg)
	base, head, ok := vacuousTestDiffRange(cfg, dir, caps)
	if !ok {
		return criterionScore{}, false
	}
	added := gitLines(dir, caps, "diff", "--name-status", "--diff-filter=A", base, head)
	if len(added) == 0 {
		return criterionScore{}, false
	}

	prodCache := map[string]map[string]bool{}
	var vacuous, checkedFiles []string
	for _, line := range added {
		_, path, cut := strings.Cut(line, "\t")
		if !cut {
			continue
		}
		lang := matchVacuousTestLang(path)
		if lang == nil {
			continue // language we don't parse - skip, never guess
		}
		content, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil || !lang.hasTestDecl.Match(content) {
			continue // unreadable, or no test declaration - e.g. a pure fixture/helper file
		}
		idents, cached := prodCache[lang.name]
		if !cached {
			idents = productionIdentifiers(dir, lang)
			prodCache[lang.name] = idents
		}
		if len(idents) == 0 {
			continue // we found zero declarations for this language - a parse gap (shallow clone,
			// unusual style, no prod files yet), not evidence the test is vacuous; don't guess
		}
		checkedFiles = append(checkedFiles, path)
		if !referencesAny(content, idents) {
			vacuous = append(vacuous, path)
		}
	}
	if len(checkedFiles) == 0 {
		return criterionScore{}, false // nothing this check knows how to evaluate
	}
	if len(vacuous) == 0 {
		return criterionScore{Score: 1, Reason: fmt.Sprintf(
			"deterministic: %d added test file(s) each reference at least one production identifier", len(checkedFiles))}, true
	}
	sort.Strings(vacuous)
	return criterionScore{Score: 0, Reason: fmt.Sprintf(
		"deterministic: %d of %d added test file(s) never name a single identifier declared in the repo's non-test "+
			"source - their assertions only touch locals/lambdas the test itself declares, so they pass even if the "+
			"code under test is deleted: %s. Fix: reference the actual production type/function each test claims to cover.",
		len(vacuous), len(checkedFiles), strings.Join(vacuous, ", "))}, true
}

// vacuousTestDiffRange: this node's own base/head, same fallback as diffSince (gitprobe.go).
func vacuousTestDiffRange(cfg Config, dir string, caps workspace.Caps) (base, head string, ok bool) {
	base = cfg.NodeBaseSHA
	if base == "" {
		var err error
		if base, err = baseCommit(dir, caps); err != nil {
			return "", "", false
		}
	}
	head = gitLine(dir, caps, "rev-parse", "HEAD")
	if head == "" || head == base {
		return "", "", false
	}
	return base, head, true
}

func matchVacuousTestLang(relPath string) *vacuousTestLang {
	base := filepath.Base(relPath)
	for i := range vacuousTestLangs {
		lang := &vacuousTestLangs[i]
		if hasAnySuffix(base, lang.ext) && lang.isTestFile(base) {
			return lang
		}
	}
	return nil
}

func hasAnySuffix(s string, suffixes []string) bool {
	for _, suf := range suffixes {
		if strings.HasSuffix(s, suf) {
			return true
		}
	}
	return false
}

// productionIdentifiers walks dir for lang's non-test files and collects declared names.
func productionIdentifiers(dir string, lang *vacuousTestLang) map[string]bool {
	idents := map[string]bool{}
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // best-effort: an unreadable entry contributes nothing, doesn't abort the walk
		}
		if d.IsDir() {
			if path != dir && (vacuousTestSkipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		base := d.Name()
		if !hasAnySuffix(base, lang.ext) || lang.isTestFile(base) {
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, m := range lang.declPattern.FindAllSubmatch(content, -1) {
			for _, g := range m[1:] {
				if len(g) >= 2 {
					idents[string(g)] = true
				}
			}
		}
		return nil
	})
	return idents
}

func referencesAny(content []byte, idents map[string]bool) bool {
	if len(idents) == 0 {
		return false
	}
	for _, tok := range identTokenRe.FindAll(content, -1) {
		if idents[string(tok)] {
			return true
		}
	}
	return false
}
