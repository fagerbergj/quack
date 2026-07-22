package vetting

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCiteFile writes content under dir/rel (creating parent dirs) and
// returns the file's line count - a small helper the disk-verified citation
// tests use to build a real, on-disk "clone" instead of faking a ledger.
func writeCiteFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRecordSearchResults(t *testing.T) {
	seen := map[string]string{}
	resp := map[string]any{"results": []any{
		map[string]any{"url": "https://a.com/x", "snippet": "snippet a", "title": "A"},
		map[string]any{"url": "https://b.com/y", "snippet": "snippet b"},
		map[string]any{"title": "no url here"}, // skipped: no url
	}}
	recordSearchResults(seen, resp)
	if seen["https://a.com/x"] != "snippet a" {
		t.Errorf("a snippet = %q, want %q", seen["https://a.com/x"], "snippet a")
	}
	if seen["https://b.com/y"] != "snippet b" {
		t.Errorf("b snippet = %q", seen["https://b.com/y"])
	}
	if len(seen) != 2 {
		t.Errorf("seen has %d entries, want 2 (url-less result skipped)", len(seen))
	}
}

func TestCitationScoreLayers(t *testing.T) {
	// One cited URL per layer: exact-fetched(1.0), exact-searched(0.75),
	// same-host-fetched(0.5), same-host-searched(0.25), unbacked(0.0).
	answer := strings.Join([]string{
		"[a](https://ex.com/fetched-page)",   // exact fetched
		"[b](https://srch.com/exact-result)", // exact searched
		"[c](https://ex.com/other)",          // same host as a fetched page
		"[d](https://srch.com/other)",        // same host as a search result
		"[e](https://nowhere.com/made-up)",   // never seen
	}, " ")
	act := workerActivity{
		fetched: map[string]fetchRecord{"https://ex.com/fetched-page": {}},
		seen:    map[string]string{"https://srch.com/exact-result": "snip"},
	}

	score, details, ok := citationScore(answer, act, nil)
	if !ok {
		t.Fatal("citationScore ok=false, want true (answer cites URLs)")
	}
	want := map[string]float64{
		"https://ex.com/fetched-page":   1.0,
		"https://srch.com/exact-result": 0.75,
		"https://ex.com/other":          0.5,
		"https://srch.com/other":        0.25,
		"https://nowhere.com/made-up":   0.0,
	}
	got := map[string]float64{}
	for _, d := range details {
		got[d.url] = d.score
	}
	for u, w := range want {
		if got[u] != w {
			t.Errorf("citation %s scored %.2f, want %.2f", u, got[u], w)
		}
	}
	wantMean := (1.0 + 0.75 + 0.5 + 0.25 + 0.0) / 5
	if score != wantMean {
		t.Errorf("mean score = %.3f, want %.3f", score, wantMean)
	}
}

func TestCitationScoreNormalizesAnchorsAndSlashes(t *testing.T) {
	// Cited URL differs from the fetched one only by a trailing slash and a #anchor
	// and host casing - should still score a 1.0 exact-fetch match.
	answer := "See [x](https://Ex.com/Page/#section)."
	act := workerActivity{fetched: map[string]fetchRecord{"https://ex.com/Page": {}}}
	score, _, ok := citationScore(answer, act, nil)
	if !ok || score != 1.0 {
		t.Errorf("score=%.2f ok=%v, want 1.0 true (anchor/slash/case normalized)", score, ok)
	}
}

// TestCitationScoreClonedRepoGrounding reenacts the live failure (2026-07-12):
// an explore-repo node cloned a repo, read files inside it via read_file, and
// cited (a) a blob URL under the cloned repo and (b) local file paths - honest,
// fully-grounded citations that scored 0.25 mean backing and sank a node the
// judge had passed. Cloned-repo grounding is full backing; fabrication
// (un-cloned repos, untouched paths) still scores 0. Local paths are now
// disk-verified against a real clone root, not the ledger - act.paths is left
// EMPTY on purpose to prove disk verification alone backs the real files.
func TestCitationScoreClonedRepoGrounding(t *testing.T) {
	root := t.TempDir()
	writeCiteFile(t, root, "games-repo/app/games.ts", "export {};\n")
	writeCiteFile(t, root, "games-repo/lib/board.ts", "export {};\n")
	writeCiteFile(t, root, "games-repo/README.md", "# games\n")

	answer := strings.Join([]string{
		"[README.md](https://github.com/fagerbergj/games/blob/main/README.md)", // URL under the cloned repo → 1.0
		"[games.ts](games-repo/app/games.ts)",                                  // real file on disk → 1.0
		"[board.ts](games-repo/lib/board.ts)",                                  // real file, never in the (empty) ledger → 1.0
		"[readme](games-repo/README.md#usage)",                                 // fragment dropped, real file → 1.0
		"[fabricated](docs/never-touched.md)",                                  // does not exist on disk → 0.0
		"[not-that-dir](games-repo-extra/x.ts)",                                // does not exist on disk → 0.0
		"[other-repo](https://github.com/fagerbergj/games-extra)",              // segment boundary: clone of …/games doesn't back …/games-extra → 0.0
		"[uncloned](https://github.com/other/thing)",                           // URL to a repo never cloned → 0.0
	}, " ")
	act := workerActivity{
		clonedRepos: []string{"https://github.com/fagerbergj/games.git"}, // .git suffix normalized away
	}

	score, details, ok := citationScore(answer, act, []string{root})
	if !ok {
		t.Fatal("citationScore abstained despite a clone root and cloned-repo URL activity")
	}
	want := map[string]float64{
		"https://github.com/fagerbergj/games/blob/main/README.md": 1.0,
		"games-repo/app/games.ts":                                 1.0,
		"games-repo/lib/board.ts":                                 1.0,
		"games-repo/README.md#usage":                              1.0,
		"docs/never-touched.md":                                   0.0,
		"games-repo-extra/x.ts":                                   0.0,
		"https://github.com/fagerbergj/games-extra":               0.0,
		"https://github.com/other/thing":                          0.0,
	}
	got := map[string]float64{}
	for _, d := range details {
		got[d.url] = d.score
	}
	for target, w := range want {
		if got[target] != w {
			t.Errorf("citation %s scored %.2f, want %.2f", target, got[target], w)
		}
	}
	if wantMean := 4.0 / 8.0; score != wantMean {
		t.Errorf("mean score = %.3f, want %.3f (details: %+v)", score, wantMean, details)
	}
}

// TestCitationScoreSkipsAnchorsAndNonWebSchemes: in-document anchors and
// mailto: targets are not citations - they must not enter the mean at all.
func TestCitationScoreSkipsAnchorsAndNonWebSchemes(t *testing.T) {
	root := t.TempDir()
	writeCiteFile(t, root, "repo/file.go", "package repo\n")
	answer := "[sec](#usage) [mail](mailto:a@b.com) [real](repo/file.go)"
	score, details, ok := citationScore(answer, workerActivity{}, []string{root})
	if !ok || len(details) != 1 || score != 1.0 {
		t.Errorf("score=%.2f details=%+v ok=%v, want 1.0 with exactly the path graded", score, details, ok)
	}
}

func TestCitationScoreNoCitations(t *testing.T) {
	_, _, ok := citationScore("A plain answer with no links.", workerActivity{}, nil)
	if ok {
		t.Error("citationScore ok=true for an answer with no URLs, want false")
	}
}

func TestCitationScoreSkippedWithoutRetrieval(t *testing.T) {
	// A non-web agent (synthesizer) does no fetch/search and has no clone root
	// to disk-verify against, so its citation can't be graded - ok=false leaves
	// the model's cites_sources in place.
	answer := "Per the research, [x](https://ex.com/a)."
	if _, _, ok := citationScore(answer, workerActivity{}, nil); ok {
		t.Error("citationScore ok=true with no retrieval or clone root, want false (skip override)")
	}
}

// TestCitationScoreCodeExplorerInlineFormat reenacts #437: the code-explorer
// cites files via "<repo>@path[:lines]" inline text, not Markdown links.
// citationScore must recognize this format and disk-verify it against the
// clone root (path exists → 1.0), not silently drop the citation to a 0.00
// mean, and not rely on a ledger the code-explorer (an external ACP agent)
// never populates.
func TestCitationScoreCodeExplorerInlineFormat(t *testing.T) {
	root := t.TempDir()
	writeCiteFile(t, root, "internal/foo.go", strings.Repeat("line\n", 10))
	answer := strings.Join([]string{
		"See quack@internal/foo.go:1-5 for the entry point.",
		"Also quack@internal/bar.go was never opened.",
	}, " ")
	score, details, ok := citationScore(answer, workerActivity{}, []string{root})
	if !ok {
		t.Fatal("citationScore ok=false, want true (code citations present)")
	}
	if len(details) != 2 {
		t.Fatalf("details = %+v, want 2 entries", details)
	}
	got := map[string]float64{}
	for _, d := range details {
		got[d.url] = d.score
	}
	if got["quack@internal/foo.go:1-5"] != 1.0 {
		t.Errorf("read file scored %.2f, want 1.0", got["quack@internal/foo.go:1-5"])
	}
	if got["quack@internal/bar.go"] != 0.0 {
		t.Errorf("untouched file scored %.2f, want 0.0", got["quack@internal/bar.go"])
	}
	if wantMean := 0.5; score != wantMean {
		t.Errorf("mean score = %.3f, want %.3f", score, wantMean)
	}
}

// TestCitationScoreDoesNotConfuseEmailForCodeCite guards the "at least one
// slash in the path" constraint on codeCiteRe: an email address inside a
// mailto: link must not be misread as an unbacked code citation.
func TestCitationScoreDoesNotConfuseEmailForCodeCite(t *testing.T) {
	root := t.TempDir()
	writeCiteFile(t, root, "repo/file.go", "package repo\n")
	answer := "[mail](mailto:a@b.com) [real](repo/file.go)"
	score, details, ok := citationScore(answer, workerActivity{}, []string{root})
	if !ok || len(details) != 1 || score != 1.0 {
		t.Errorf("score=%.2f details=%+v ok=%v, want 1.0 with exactly the path graded (email not counted)", score, details, ok)
	}
}

// TestCitationScoreDiskVerifiesBareAndRepoPrefixedForm covers (a) from the
// #437 rework: a code cite to a real file scores 1.0 whether written bare
// (repo-relative) or in the code-explorer's "<repo>@path" inline form.
func TestCitationScoreDiskVerifiesBareAndRepoPrefixedForm(t *testing.T) {
	root := t.TempDir()
	writeCiteFile(t, root, "internal/foo.go", strings.Repeat("line\n", 10))

	answer := "See [foo](internal/foo.go) and quack@internal/foo.go for the entry point."
	score, details, ok := citationScore(answer, workerActivity{}, []string{root})
	if !ok {
		t.Fatal("citationScore ok=false, want true")
	}
	if len(details) != 2 {
		t.Fatalf("details = %+v, want 2 entries (bare markdown link + code cite)", details)
	}
	if score != 1.0 {
		t.Errorf("score = %.2f, want 1.0 (both forms cite a real file)", score)
	}
}

// TestCitationScoreDiskVerificationCatchesFabrication covers (b): a code
// citation to a file that does not exist on disk scores 0.0 even with a
// valid clone root passed.
func TestCitationScoreDiskVerificationCatchesFabrication(t *testing.T) {
	root := t.TempDir()
	writeCiteFile(t, root, "internal/foo.go", "package foo\n")

	answer := "See quack@internal/never-existed.go for the entry point."
	score, details, ok := citationScore(answer, workerActivity{}, []string{root})
	if !ok {
		t.Fatal("citationScore ok=false, want true (clone root present)")
	}
	if len(details) != 1 || score != 0.0 {
		t.Errorf("score=%.2f details=%+v, want 0.0 for a file that doesn't exist", score, details)
	}
}

// TestCitationScoreDiskVerificationCatchesOutOfRangeLine covers (c): a cited
// line range beyond the file's actual line count is fabricated even though
// the file itself exists.
func TestCitationScoreDiskVerificationCatchesOutOfRangeLine(t *testing.T) {
	root := t.TempDir()
	writeCiteFile(t, root, "internal/foo.go", strings.Repeat("line\n", 10)) // 10 lines

	answer := "See quack@internal/foo.go:9999 for the entry point."
	score, details, ok := citationScore(answer, workerActivity{}, []string{root})
	if !ok {
		t.Fatal("citationScore ok=false, want true (clone root present)")
	}
	if len(details) != 1 || score != 0.0 {
		t.Errorf("score=%.2f details=%+v, want 0.0 for a line range past the file's end", score, details)
	}
}

// TestCitationScoreEmptyLedgerWithCloneRootStillDiskVerifies covers (d), the
// regression that matters: a setup-provisioned node whose ledger is
// completely EMPTY (no act.paths, no act.clonedDirs - exactly what a
// harness-provisioned clone or an external ACP agent leaves behind, #437)
// still scores real citations 1.0 as long as a clone root is passed in.
func TestCitationScoreEmptyLedgerWithCloneRootStillDiskVerifies(t *testing.T) {
	root := t.TempDir() // stands in for the resolved SetupCloneDir
	writeCiteFile(t, root, "internal/foo.go", strings.Repeat("line\n", 10))

	answer := "The entry point is [internal/foo.go](internal/foo.go)."
	score, _, ok := citationScore(answer, workerActivity{}, []string{root})
	if !ok {
		t.Fatal("citationScore abstained despite a clone root - an empty ledger must not cause abstention")
	}
	if score != 1.0 {
		t.Errorf("score = %.2f, want 1.0 (real file, empty ledger, clone root passed)", score)
	}
}

// TestCitationScoreRejectsPathEscape covers (f): a cited path that tries to
// escape the clone root via ".." must never be disk-verified, regardless of
// what actually exists at the resolved location outside the root.
func TestCitationScoreRejectsPathEscape(t *testing.T) {
	root := t.TempDir()
	// A real /etc/passwd exists on the host - prove the escape is rejected
	// on containment, not because the target happens to be missing.
	answer := "[passwd](../../etc/passwd)"
	score, details, ok := citationScore(answer, workerActivity{}, []string{root})
	if !ok {
		t.Fatal("citationScore ok=false, want true (clone root present)")
	}
	if len(details) != 1 || score != 0.0 {
		t.Errorf("score=%.2f details=%+v, want 0.0 - a path escape must never be backed", score, details)
	}
}

func TestLengthScore(t *testing.T) {
	for _, c := range []struct {
		in   string
		want float64
	}{
		{"", 0.0},
		{"   \n\t ", 0.0},
		{"a", 1.0},
		{"a full enough answer", 1.0},
	} {
		if got := lengthScore(c.in); got != c.want {
			t.Errorf("lengthScore(%q) = %.1f, want %.1f", c.in, got, c.want)
		}
	}
}

func TestNormalizeURL(t *testing.T) {
	for _, c := range []struct {
		in       string
		wantNorm string
		wantHost string
	}{
		{"https://Ex.com/Page/#section", "https://ex.com/Page", "ex.com"},
		{"http://A.com/", "http://a.com/", "a.com"},          // root path kept
		{"https://b.com/x/y/", "https://b.com/x/y", "b.com"}, // trailing slash trimmed
		{"not a url", "not a url", ""},                       // parse fallback
	} {
		gotNorm, gotHost := normalizeURL(c.in)
		if gotNorm != c.wantNorm || gotHost != c.wantHost {
			t.Errorf("normalizeURL(%q) = (%q,%q), want (%q,%q)", c.in, gotNorm, gotHost, c.wantNorm, c.wantHost)
		}
	}
}

func TestParseVerdictToleratesFencedJSON(t *testing.T) {
	// Fenced block is stripped, and the 0–10 score is normalized to 0–1.
	v, err := parseVerdict("```json\n{\"score\": 8, \"passed\": true, \"feedback\": \"x\"}\n```")
	if err != nil {
		t.Fatal(err)
	}
	if v.Score != 0.8 {
		t.Errorf("score = %v, want 0.8 (8/10 normalized)", v.Score)
	}
}

func TestParseVerdictMisplacedTopLevel(t *testing.T) {
	// Reproduces the exact failure seen in prod: the model nested score/passed/feedback
	// inside criteria and omitted the outer closing brace.
	malformed := `{"criteria":{"grounded":{"reason":"good","score":0.9},"no_fabrication":{"reason":"ok","score":1.0},"answers_question":{"reason":"yes","score":1.0},"internally_consistent":{"reason":"fine","score":0.9},"cites_sources":{"reason":"none","score":0.0},"score":0.76,"passed":true,"feedback":"add citations"}`

	v, err := parseVerdict(malformed)
	if err != nil {
		t.Fatalf("parseVerdict(misplaced): %v", err)
	}
	// cites_sources=0 is the lowest criterion → overall score is that minimum (0.0)
	if v.Score != 0 {
		t.Errorf("score = %.2f, want 0 (lowest criterion: cites_sources=0)", v.Score)
	}
	// Feedback recovered from misplaced entry
	if v.Feedback != "add citations" {
		t.Errorf("feedback = %q, want recovered from criteria", v.Feedback)
	}
	// The 5 real criteria should be present; score/passed/feedback should not
	for _, want := range []string{"grounded", "no_fabrication", "answers_question", "internally_consistent", "cites_sources"} {
		if _, ok := v.Criteria[want]; !ok {
			t.Errorf("criteria missing %q", want)
		}
	}
	for _, bad := range []string{"score", "passed", "feedback"} {
		if _, ok := v.Criteria[bad]; ok {
			t.Errorf("criteria should not contain %q", bad)
		}
	}
}

func TestParseVerdictDuplicatedBlob(t *testing.T) {
	// Model emitted the JSON object twice (back-to-back); only the first should be parsed.
	blob := `{"score":0.8,"passed":true,"feedback":"ok"}`
	v, err := parseVerdict(blob + blob)
	if err != nil {
		t.Fatalf("parseVerdict(duplicated): %v", err)
	}
	if !v.Passed || v.Score != 0.8 {
		t.Errorf("unexpected verdict: %+v", v)
	}
}

func TestParseVerdictLowestCriterion(t *testing.T) {
	// Well-formed G-Eval verdict; the overall score is the lowest criterion, so
	// cites_sources=0 sinks it to 0.0 regardless of the model's holistic 0.96.
	input := `{"criteria":{"grounded":{"score":0.9},"no_fabrication":{"score":1.0},"answers_question":{"score":1.0},"internally_consistent":{"score":0.9},"cites_sources":{"score":0.0}},"score":0.96,"passed":true,"feedback":"no sources"}`
	v, err := parseVerdict(input)
	if err != nil {
		t.Fatal(err)
	}
	if v.Score != 0 {
		t.Errorf("score = %.2f, want 0 (lowest criterion: cites_sources=0)", v.Score)
	}
}

func TestAggregateVerdictMinAndClamp(t *testing.T) {
	// Per-criterion gating: the overall is the WEAKEST criterion (cites_sources=0),
	// not the mean, and not the model's submitted 0.96.
	v := aggregateVerdict(verdict{Criteria: map[string]criterionScore{
		"grounded":              {Score: 0.9},
		"no_fabrication":        {Score: 1.0},
		"answers_question":      {Score: 1.0},
		"internally_consistent": {Score: 0.9},
		"cites_sources":         {Score: 0.0},
	}, Score: 0.96})
	if v.Score != 0 {
		t.Errorf("score = %.2f, want 0 (lowest criterion)", v.Score)
	}
	// All-strong criteria → overall is the lowest of them.
	if got := aggregateVerdict(verdict{Criteria: map[string]criterionScore{
		"a": {Score: 0.8}, "b": {Score: 0.9}, "c": {Score: 0.7},
	}}).Score; got != 0.7 {
		t.Errorf("min: score = %v, want 0.7", got)
	}
	// No criteria: the submitted score is kept but clamped to [0,1].
	if got := aggregateVerdict(verdict{Score: 1.5}).Score; got != 1 {
		t.Errorf("clamp high: score = %v, want 1", got)
	}
	if got := aggregateVerdict(verdict{Score: -0.2}).Score; got != 0 {
		t.Errorf("clamp low: score = %v, want 0", got)
	}
}

// The rubric asks the judge for 0–10 integers; the pipeline works in 0–1. A
// verdict on the 0–10 scale (detected by any score > 1) must be divided by 10,
// so a perfect criterion (10) becomes 1.0 and the weakest drives the overall.
func TestParseVerdictNormalizes0To10Scale(t *testing.T) {
	input := `{"criteria":{"grounded":{"score":9},"no_fabrication":{"score":10},"answers_question":{"score":8},"internally_consistent":{"score":9},"cites_sources":{"score":6}},"score":8,"passed":true,"feedback":""}`
	v, err := parseVerdict(input)
	if err != nil {
		t.Fatal(err)
	}
	// Lowest criterion is cites_sources=6 → 0.6 after /10.
	if v.Score != 0.6 {
		t.Errorf("score = %v, want 0.6 (lowest criterion 6/10)", v.Score)
	}
	if g := v.Criteria["grounded"].Score; g != 0.9 {
		t.Errorf("grounded = %v, want 0.9 (9/10)", g)
	}
}

// normalizeScale must leave a verdict already on the 0–1 axis untouched (some
// models ignore the 0–10 instruction and answer in 0–1).
func TestNormalizeScaleLeaves0To1Untouched(t *testing.T) {
	v := verdict{Score: 0.9, Criteria: map[string]criterionScore{"a": {Score: 0.8}, "b": {Score: 0.5}}}
	normalizeScale(&v)
	if v.Score != 0.9 || v.Criteria["a"].Score != 0.8 || v.Criteria["b"].Score != 0.5 {
		t.Errorf("0–1 verdict was altered: %+v", v)
	}
}

// TestFoldDeterministic_RequireRetrievalHardFail: a retrieval agent that did
// ZERO web_search/web_fetch cannot pass the gate - regression for the live e2e
// 2026-07-05 hole where a worker wrote a question to the user as its answer
// text (no tool calls at all), citationScore abstained (nothing to grade), and
// the judge waved the "answer" through. Weakest-link must be 0, and the
// feedback must point at BOTH ways out (retrieve, or ask_user).
func TestFoldDeterministic_RequireRetrievalHardFail(t *testing.T) {
	v := verdict{Criteria: map[string]criterionScore{"accuracy": {Score: 0.9}}}
	got := foldDeterministic(context.Background(), v, "Which city are you moving to?", workerActivity{}, Config{RequireRetrieval: true})
	if got.Score != 0 {
		t.Fatalf("score = %v, want 0 (weakest-link on grounded_in_retrieval)", got.Score)
	}
	c, ok := got.Criteria["grounded_in_retrieval"]
	if !ok || c.Score != 0 {
		t.Fatalf("grounded_in_retrieval criterion missing or nonzero: %+v", got.Criteria)
	}
	if !strings.Contains(c.Reason, "ask_user") {
		t.Errorf("feedback should name ask_user as the way out when blocked on the user; got %q", c.Reason)
	}
}

// TestFoldDeterministic_NoRetrievalOKForSynthesizer: a tool-less agent
// (RequireRetrieval=false) with no activity is NOT penalized - it legitimately
// re-cites upstream URLs (the pre-existing citationScore abstention stands).
func TestFoldDeterministic_NoRetrievalOKForSynthesizer(t *testing.T) {
	v := verdict{Criteria: map[string]criterionScore{"accuracy": {Score: 0.9}}}
	got := foldDeterministic(context.Background(), v, "Combined findings: [x](https://ex.com/a).", workerActivity{}, Config{})
	if _, present := got.Criteria["grounded_in_retrieval"]; present {
		t.Fatal("grounded_in_retrieval applied to a non-retrieval agent")
	}
	if got.Score != 0.9 {
		t.Errorf("score = %v, want 0.9 (untouched)", got.Score)
	}
}

// TestFoldDeterministic_WorkspaceGroundingSatisfiesRetrieval: a coding node
// that consulted the repo on disk (clone and/or reads) instead of the web is
// grounded - grounded_in_retrieval must not fire on zero web activity alone.
func TestFoldDeterministic_WorkspaceGroundingSatisfiesRetrieval(t *testing.T) {
	for name, act := range map[string]workerActivity{
		"clone": {clonedRepos: []string{"https://github.com/org/repo"}, clonedDirs: []string{"repo"}},
		"reads": {paths: map[string]bool{"repo/main.go": true}},
	} {
		v := verdict{Criteria: map[string]criterionScore{"accuracy": {Score: 0.9}}}
		got := foldDeterministic(context.Background(), v, "The entrypoint is [main.go](repo/main.go).", act, Config{RequireRetrieval: true})
		if _, present := got.Criteria["grounded_in_retrieval"]; present {
			t.Errorf("%s: grounded_in_retrieval penalty applied despite workspace grounding", name)
		}
	}
}

// TestFoldDeterministic_RetrievalPresentNotPenalized: any recorded retrieval
// (even just search results seen) satisfies the grounding check; citation
// backing is then graded by citationScore as before.
func TestFoldDeterministic_RetrievalPresentNotPenalized(t *testing.T) {
	act := workerActivity{seen: map[string]string{"https://ex.com/a": "snippet"}}
	v := verdict{Criteria: map[string]criterionScore{"accuracy": {Score: 0.9}}}
	got := foldDeterministic(context.Background(), v, "Answer citing [x](https://ex.com/a).", act, Config{RequireRetrieval: true})
	if _, present := got.Criteria["grounded_in_retrieval"]; present {
		t.Fatal("grounded_in_retrieval penalty applied despite recorded retrieval")
	}
}

// TestFoldDeterministic_ExplorationGroundedCatchesFabrication: the #289 live
// failure - a code-explorer node clones a repo, performs ZERO read_file/grep
// calls, and still emits a confident, substantive "survey" of it. The clone
// alone satisfies grounded_in_retrieval, so exploration_grounded is the
// backstop that must sink the score to 0 (weakest-link) regardless of how
// good the judge's other criteria look.
func TestFoldDeterministic_ExplorationGroundedCatchesFabrication(t *testing.T) {
	act := workerActivity{clonedRepos: []string{"https://github.com/org/repo"}, clonedDirs: []string{"repo"}}
	v := verdict{Criteria: map[string]criterionScore{"accuracy": {Score: 0.9}}}
	cfg := Config{ExternalWorker: true, ReadOnly: true}
	got := foldDeterministic(context.Background(), v, "internal/engine/ is a 319k-line monolith spanning...", act, cfg)
	if got.Score != 0 {
		t.Fatalf("score = %v, want 0 (weakest-link on exploration_grounded)", got.Score)
	}
	c, ok := got.Criteria["exploration_grounded"]
	if !ok || c.Score != 0 {
		t.Fatalf("exploration_grounded criterion missing or nonzero: %+v", got.Criteria)
	}
}

// TestFoldDeterministic_ExplorationGroundedPassesWithReads: a code-explorer
// that actually read (or grepped) the clone is not penalized - acceptance
// case from #289.
func TestFoldDeterministic_ExplorationGroundedPassesWithReads(t *testing.T) {
	cfg := Config{ExternalWorker: true, ReadOnly: true}
	for name, act := range map[string]workerActivity{
		"reads": {clonedRepos: []string{"https://github.com/org/repo"}, paths: map[string]bool{"repo/main.go": true}},
		"greps": {clonedRepos: []string{"https://github.com/org/repo"}, greps: 3},
	} {
		v := verdict{Criteria: map[string]criterionScore{"accuracy": {Score: 0.9}}}
		got := foldDeterministic(context.Background(), v, "main.go is the entrypoint; it wires up the router.", act, cfg)
		if _, present := got.Criteria["exploration_grounded"]; present {
			t.Errorf("%s: exploration_grounded penalty applied despite real exploration activity", name)
		}
	}
}

// TestFoldDeterministic_ExplorationGroundedScopedToExternalReadOnly: the
// check must not fire outside its scope - a node with no clone at all
// (legitimately read-nothing) and a non-ReadOnly / non-ExternalWorker agent
// (e.g. a native synthesizer with a bare clone in its activity, which cannot
// happen in practice but must still be inert) are both untouched.
func TestFoldDeterministic_ExplorationGroundedScopedToExternalReadOnly(t *testing.T) {
	cloned := workerActivity{clonedRepos: []string{"https://github.com/org/repo"}}
	cases := map[string]Config{
		"not external":  {ReadOnly: true},
		"not read-only": {ExternalWorker: true},
		"neither":       {},
	}
	for name, cfg := range cases {
		v := verdict{Criteria: map[string]criterionScore{"accuracy": {Score: 0.9}}}
		got := foldDeterministic(context.Background(), v, "some findings", cloned, cfg)
		if _, present := got.Criteria["exploration_grounded"]; present {
			t.Errorf("%s: exploration_grounded fired out of scope", name)
		}
	}
	// No clone at all: an ExternalWorker+ReadOnly node that never cloned
	// anything (e.g. it worked in a pre-provisioned setup clone) has nothing
	// to be ungrounded about.
	v := verdict{Criteria: map[string]criterionScore{"accuracy": {Score: 0.9}}}
	got := foldDeterministic(context.Background(), v, "some findings", workerActivity{}, Config{ExternalWorker: true, ReadOnly: true})
	if _, present := got.Criteria["exploration_grounded"]; present {
		t.Error("exploration_grounded fired with no clone at all")
	}
}
