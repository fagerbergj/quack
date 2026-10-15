// dataset_seed.go: `quack dataset seed` plants one known defect in each reviewed PR of a dataset, so
// `experiment run` can score a reviewer's recall against it in code (seeded_hit) instead of with a judge.
package cli

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/scanner"
	"go/token"
	"hash/fnv"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/fagerbergj/quack/internal/langfuse"
)

// SeedOpts names the source and target datasets and the local bare mirror of the items' repository.
type SeedOpts struct {
	Source, Target, Mirror string
	Limit                  int
}

// SeedItem is one source item's outcome: the item seeded from it, or why it was skipped.
type SeedItem struct {
	SourceID, ItemID, Skipped string
	Defect                    seededDefect
}

// seededDefect is the one mutation planted in an item, as its metadata.seeded records it.
type seededDefect struct {
	Path     string `json:"path"`
	Line     int    `json:"line"`
	Operator string `json:"operator"`
	Original string `json:"original"`
	Mutated  string `json:"mutated"`
	end      int    // drop_err_guard's closing-brace line; 0 for a one-line rewrite or deletion
}

// RunDatasetSeed writes one seeded item to opts.Target per opts.Source item it can mutate. Each seeded PR is
// served by its own tiny bare repo under <mirror dir>/items/, borrowing the mirror's objects through alternates.
func RunDatasetSeed(ctx context.Context, lf *langfuse.Client, opts SeedOpts) ([]SeedItem, error) {
	items, err := listDatasetItems(ctx, lf, opts.Source, opts.Limit)
	if err != nil {
		return nil, err
	}
	mirror, err := filepath.Abs(opts.Mirror)
	if err != nil {
		return nil, err
	}
	var out []SeedItem
	ensured := false
	for _, item := range items {
		req, d, err := seedItem(mirror, opts.Target, item)
		if err != nil {
			out = append(out, SeedItem{SourceID: item.ID, Skipped: err.Error()})
			continue
		}
		if !ensured {
			if err := ensureDataset(ctx, lf, opts.Target); err != nil {
				return out, err
			}
			ensured = true
		}
		if err := lf.CreateDatasetItem(ctx, req); err != nil {
			return out, fmt.Errorf("dataset seed: create item from %s: %w", item.ID, err)
		}
		out = append(out, SeedItem{SourceID: item.ID, ItemID: req.ID, Defect: d})
	}
	return out, nil
}

// seedItem plants item's defect in the mirror and builds the seeded item; any error skips just this item.
func seedItem(mirror, target string, item langfuse.DatasetItem) (langfuse.CreateDatasetItemRequest, seededDefect, error) {
	var in datasetItemInput
	var pull struct {
		Base struct{ SHA string } `json:"base"`
	}
	if err := remarshal(item.Input, &in); err != nil {
		return langfuse.CreateDatasetItemRequest{}, seededDefect{}, err
	}
	_ = json.Unmarshal([]byte(in.Artifacts["pull"]), &pull)
	if !safeItemID.MatchString(item.ID) || in.Setup == nil || in.Setup.ExistingHeadRef == "" ||
		!seedSHA.MatchString(in.Setup.HeadSHA) || !seedSHA.MatchString(pull.Base.SHA) {
		return langfuse.CreateDatasetItemRequest{}, seededDefect{}, errors.New("no pinned PR head and base (input.setup, the pull artifact), or an id unsafe as a path")
	}
	head := in.Setup.HeadSHA
	p, err := plantDefect(mirror, item.ID, in.Setup.Repo, head, pull.Base.SHA)
	if err != nil {
		return langfuse.CreateDatasetItemRequest{}, seededDefect{}, err
	}
	repo, err := itemRepo(mirror, item.ID, map[string]string{in.Setup.BaseRef: p.base, in.Setup.ExistingHeadRef: p.seeded})
	if err != nil {
		return langfuse.CreateDatasetItemRequest{}, seededDefect{}, err
	}
	setup := *in.Setup
	setup.Repo, setup.HeadSHA = "file://"+repo, p.seeded
	in.Setup = &setup
	in.Task = strings.ReplaceAll(in.Task, head, p.seeded)
	for name, body := range in.Artifacts {
		if name == "files" {
			body = reseedFiles(body, p.defect.Path, p.patch)
		}
		in.Artifacts[name] = strings.ReplaceAll(body, head, p.seeded)
	}
	meta := map[string]any{}
	if m, ok := item.Metadata.(map[string]any); ok {
		maps.Copy(meta, m)
	}
	meta["seeded"], meta["source_item"] = p.defect, item.ID
	// exportItemID's hash keeps the seeded item's id stable per (target, source item), so a re-seed upserts.
	req := langfuse.CreateDatasetItemRequest{DatasetName: target, ID: exportItemID(target, item.ID, "seeded", ""), Input: &in, Metadata: meta}
	return req, p.defect, nil
}

// seedSHA and safeItemID gate item-supplied values before they reach git argv and the mirror's paths.
var (
	seedSHA    = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	safeItemID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

// planted is one defect committed to the mirror: the PR's merge base, the seeded head, and the file's new PR patch.
type planted struct {
	defect              seededDefect
	base, seeded, patch string
}

// plantDefect picks itemID's mutation among the Go lines the PR (base...head) added and commits it to the mirror.
func plantDefect(mirror, itemID, upstream, head, base string) (planted, error) {
	if err := ensureMirror(mirror, upstream, head, base); err != nil {
		return planted{}, err
	}
	g := &mirrorGit{dir: mirror}
	mb := strings.TrimSpace(g.run("", "merge-base", base, head))
	cands := mutationCandidates(parseAddedLines(g.run("", "diff", "--no-color", "--no-ext-diff", "--dst-prefix=b/", "-U0", mb, head)))
	if g.err != nil {
		return planted{}, g.err
	}
	if len(cands) == 0 {
		return planted{}, errors.New("no mutable Go line among the PR's added lines")
	}
	d := pickDefect(cands, itemID)
	content, err := applyDefect(g.run("", "cat-file", "blob", head+":"+d.Path), d)
	if err != nil {
		return planted{}, err
	}
	seeded := commitDefect(g, head, d.Path, content)
	// Mirror refs keep the planted objects reachable: the item repos only borrow them.
	g.run("", "update-ref", "refs/seeded/"+itemID+"/head", seeded)
	g.run("", "update-ref", "refs/seeded/"+itemID+"/base", mb)
	diff := g.run("", "diff", "--no-color", "--no-ext-diff", mb, seeded, "--", d.Path)
	if i := strings.Index(diff, "@@ "); i >= 0 {
		diff = strings.TrimSuffix(diff[i:], "\n")
	}
	return planted{defect: d, base: mb, seeded: seeded, patch: diff}, g.err
}

// ensureMirror clones upstream as a bare mirror when there is none yet, then fetches any of shas it lacks.
func ensureMirror(mirror, upstream string, shas ...string) error {
	g := &mirrorGit{dir: filepath.Dir(mirror)}
	if _, err := os.Stat(mirror); errors.Is(err, fs.ErrNotExist) {
		g.run("", "clone", "--quiet", "--mirror", "--", upstream, mirror)
	}
	g.dir = mirror
	for _, sha := range shas {
		if g.err == nil && exec.Command("git", "-C", mirror, "cat-file", "-e", sha+"^{commit}").Run() != nil {
			g.run("", "fetch", "--quiet", "origin", sha)
		}
	}
	return g.err
}

// commitDefect rewrites head in place, same parents, author, dates and message, with path's content swapped:
// the PR's history shows no extra commit, and a re-seed reproduces the same sha.
func commitDefect(g *mirrorGit, head, path, content string) string {
	f := strings.SplitN(g.run("", "log", "-1", "--format=%an%x00%ae%x00%aI%x00%cn%x00%ce%x00%cI%x00%P%x00%B", head), "\x00", 8)
	tmp, err := os.MkdirTemp("", "quack-seed-")
	if err != nil || len(f) < 8 {
		g.err = cmp.Or(g.err, err, errors.New("unreadable head commit"))
		return ""
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	blob := strings.TrimSpace(g.run(content, "hash-object", "-w", "--stdin"))
	idx := &mirrorGit{dir: g.dir, env: []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index")}, err: g.err}
	idx.run("", "read-tree", head)
	idx.run("", "update-index", "--cacheinfo", "100644,"+blob+","+path)
	args := []string{"commit-tree", "--no-gpg-sign", strings.TrimSpace(idx.run("", "write-tree"))}
	for _, p := range strings.Fields(f[6]) {
		args = append(args, "-p", p)
	}
	idx.env = append(idx.env, "GIT_AUTHOR_NAME="+f[0], "GIT_AUTHOR_EMAIL="+f[1], "GIT_AUTHOR_DATE="+f[2],
		"GIT_COMMITTER_NAME="+f[3], "GIT_COMMITTER_EMAIL="+f[4], "GIT_COMMITTER_DATE="+f[5])
	seeded := strings.TrimSpace(idx.run(f[7], args...))
	g.err = idx.err
	return seeded
}

// itemRepo is itemID's bare repo with exactly refs (branch -> sha), its objects borrowed from the mirror, so a
// replay clones the PR's own base and head branch names and nothing that names the seeding.
func itemRepo(mirror, itemID string, refs map[string]string) (string, error) {
	repo := filepath.Join(filepath.Dir(mirror), "items", itemID+".git")
	g := &mirrorGit{dir: filepath.Dir(mirror)}
	g.run("", "init", "--quiet", "--bare", repo)
	if g.err == nil {
		g.err = os.WriteFile(filepath.Join(repo, "objects", "info", "alternates"), []byte(filepath.Join(mirror, "objects")+"\n"), 0o644)
	}
	g.dir = repo
	for _, branch := range slices.Sorted(maps.Keys(refs)) {
		g.run("", "update-ref", "refs/heads/"+branch, refs[branch])
	}
	return repo, g.err
}

// mirrorGit runs the operator's own git in dir, keeping the first error so a sequence reads straight through.
type mirrorGit struct {
	dir string
	env []string
	err error
}

func (g *mirrorGit) run(stdin string, args ...string) string {
	if g.err != nil {
		return ""
	}
	cmd := exec.Command("git", append([]string{"-C", g.dir}, args...)...)
	cmd.Env = append(os.Environ(), g.env...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
		}
		g.err = fmt.Errorf("git %s: %w", args[0], err)
	}
	return string(out)
}

// addedLine is one line a unified diff adds: its path and line number on the new side.
type addedLine struct {
	path string
	line int
	text string
}

var hunkRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)`)

// parseAddedLines reads a -U0 diff, where every hunk line is a removal or an addition.
func parseAddedLines(diff string) []addedLine {
	var out []addedLine
	path, n := "", 0
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "+++ "):
			path = strings.TrimPrefix(l[4:], "b/")
		case strings.HasPrefix(l, "@@ "):
			if m := hunkRe.FindStringSubmatch(l); m != nil {
				n, _ = strconv.Atoi(m[1])
			}
		case strings.HasPrefix(l, "+"):
			out = append(out, addedLine{path: path, line: n, text: l[1:]})
			n++
		}
	}
	return out
}

// mutationCandidates is every mutation of an added non-test Go line, in diff order, so a pick by index is stable.
func mutationCandidates(added []addedLine) []seededDefect {
	var out []seededDefect
	for i, a := range added {
		if !strings.HasSuffix(a.path, ".go") || strings.HasSuffix(a.path, "_test.go") {
			continue
		}
		for _, m := range lineMutations(a.text) {
			m.Path, m.Line, m.Original = a.path, a.line, a.text
			out = append(out, m)
		}
		if end := guardEnd(added, i); end > 0 {
			out = append(out, seededDefect{Path: a.path, Line: a.line, Operator: "drop_err_guard", Original: a.text, end: end})
		}
	}
	return out
}

// opTier ranks operators by how readily a reviewer catches them: unlisted subtle ones 0, drop_err_guard (which
// go build catches) last. A pick draws only from the lowest tier the PR offers.
var opTier = map[string]int{"flip_eq": 1, "boundary": 1, "drop_not": 1, "return_nil": 1, "drop_err_guard": 2}

// pickDefect is itemID's candidate within the subtlest tier present, chosen by hash so a re-seed repeats it.
func pickDefect(cands []seededDefect, itemID string) seededDefect {
	best := slices.MinFunc(cands, func(a, b seededDefect) int { return opTier[a.Operator] - opTier[b.Operator] })
	pool := slices.DeleteFunc(slices.Clone(cands), func(d seededDefect) bool { return opTier[d.Operator] != opTier[best.Operator] })
	h := fnv.New64a()
	_, _ = h.Write([]byte(itemID))
	return pool[h.Sum64()%uint64(len(pool))]
}

// flips: each one-token operator's mutant; == and != swap meaning, && and || swap logic, the rest move a boundary.
var flips = map[token.Token][2]string{
	token.EQL: {"flip_eq", "!="}, token.NEQ: {"flip_eq", "=="}, token.LAND: {"and_or", "||"}, token.LOR: {"and_or", "&&"},
	token.LSS: {"boundary", "<="}, token.LEQ: {"boundary", "<"}, token.GTR: {"boundary", ">="}, token.GEQ: {"boundary", ">"},
}

// lineMutations is every mutant of one line; a deletion (drop_defer) has an empty Mutated.
func lineMutations(text string) []seededDefect {
	ts := scanLine(text)
	var out []seededDefect
	for i, t := range ts {
		if f, ok := flips[t.tok]; ok {
			out = append(out, seededDefect{Operator: f[0], Mutated: text[:t.off] + f[1] + text[t.off+len(t.tok.String()):]})
		}
		if t.tok == token.NOT {
			out = append(out, seededDefect{Operator: "drop_not", Mutated: text[:t.off] + text[t.off+1:]})
		}
		if m := cmp.Or(dropOne(text, ts, i), lenMinusOne(text, ts, i)); m != "" {
			out = append(out, seededDefect{Operator: "off_by_one", Mutated: m})
		}
	}
	if n := len(ts); n > 0 && ts[0].tok == token.RETURN && ts[n-1].tok == token.IDENT && ts[n-1].lit == "err" {
		out = append(out, seededDefect{Operator: "return_nil", Mutated: text[:ts[n-1].off] + "nil" + text[ts[n-1].off+3:]})
	}
	if deferRelease(ts) {
		out = append(out, seededDefect{Operator: "drop_defer"})
	}
	return out
}

// lineTok is one Go token of a line: its byte offset, kind and literal.
type lineTok struct {
	off int
	tok token.Token
	lit string
}

// scanLine scans text as Go tokens, so an operator inside a string or comment is never mutated. The semicolon
// the scanner inserts at the line's end is dropped.
func scanLine(text string) []lineTok {
	var s scanner.Scanner
	fset := token.NewFileSet()
	file := fset.AddFile("", fset.Base(), len(text))
	s.Init(file, []byte(text), nil, 0)
	var out []lineTok
	for {
		pos, tok, lit := s.Scan()
		if tok == token.EOF {
			return out
		}
		if tok != token.SEMICOLON || lit == ";" {
			out = append(out, lineTok{file.Offset(pos), tok, lit})
		}
	}
}

// operandEnd marks tokens that can end a binary operator's left operand, so a `- 1` after one is not unary.
var operandEnd = map[token.Token]bool{token.IDENT: true, token.RPAREN: true, token.RBRACK: true}

// tighter reports ts[k] as a binary operator binding tighter than + and -, which an added or dropped 1 would split.
func tighter(ts []lineTok, k int) bool {
	return k < len(ts) && ts[k].tok.Precedence() > token.ADD.Precedence()
}

// dropOne drops a binary `+ 1` or `- 1` at ts[i] (`a[i+1]` -> `a[i]`); "" when ts[i] starts none.
func dropOne(text string, ts []lineTok, i int) string {
	t := ts[i]
	if (t.tok != token.ADD && t.tok != token.SUB) || i == 0 || i+1 >= len(ts) || !operandEnd[ts[i-1].tok] ||
		ts[i+1].tok != token.INT || ts[i+1].lit != "1" || tighter(ts, i+2) {
		return ""
	}
	return strings.TrimRight(text[:t.off], " \t") + text[ts[i+1].off+1:]
}

// lenMinusOne appends -1 to a len(...) at ts[i] that bounds an index, a slice or a loop; "" otherwise.
func lenMinusOne(text string, ts []lineTok, i int) string {
	if ts[i].lit != "len" || i+1 >= len(ts) || ts[i+1].tok != token.LPAREN || (i > 0 && ts[i-1].tok == token.PERIOD) {
		return ""
	}
	j, depth := i+1, 0
	for ; j < len(ts); j++ {
		switch ts[j].tok {
		case token.LPAREN:
			depth++
		case token.RPAREN:
			depth--
		}
		if depth == 0 {
			break
		}
	}
	if j == len(ts) || tighter(ts, j+1) || !boundsLen(tokAt(ts, i-1), tokAt(ts, j+1)) {
		return ""
	}
	return text[:ts[j].off+1] + "-1" + text[ts[j].off+1:]
}

// boundsLen reports a len(...) between prev and next as a whole index or slice bound, or a loop's `i < len(x)`;
// `len(x) == 0` and the like are left alone, as their mutant reads as planted.
func boundsLen(prev, next token.Token) bool {
	slice := (prev == token.LBRACK || prev == token.COLON) && (next == token.RBRACK || next == token.COLON)
	return slice || prev == token.LSS || prev == token.LEQ
}

// tokAt is ts[k]'s kind, ILLEGAL off either end.
func tokAt(ts []lineTok, k int) token.Token {
	if k < 0 || k >= len(ts) {
		return token.ILLEGAL
	}
	return ts[k].tok
}

// deferRelease reports a `defer x.Close()` line, a zero-arg method call: deleting it leaks or deadlocks yet
// compiles. A bare `defer cancel()` is left alone, since dropping it leaves cancel unused and fails the build.
func deferRelease(ts []lineTok) bool {
	n := len(ts)
	if n < 6 || n%2 != 0 || ts[0].tok != token.DEFER || ts[n-2].tok != token.LPAREN || ts[n-1].tok != token.RPAREN {
		return false
	}
	for k := 1; k < n-2; k++ {
		if ts[k].tok != [2]token.Token{token.IDENT, token.PERIOD}[(k-1)%2] {
			return false
		}
	}
	return true
}

// guardEnd is the closing-brace line of an `if err != nil {` block at added[i] that the PR added whole; 0 otherwise.
func guardEnd(added []addedLine, i int) int {
	a := added[i]
	if strings.TrimSpace(a.text) != "if err != nil {" {
		return 0
	}
	indent := a.text[:len(a.text)-len(strings.TrimLeft(a.text, " \t"))]
	for j := i + 1; j < len(added) && added[j].path == a.path && added[j].line == a.line+j-i; j++ {
		if added[j].text == indent+"}" {
			return added[j].line
		}
	}
	return 0
}

// applyDefect rewrites d's line in content, or deletes lines Line..end when d has no Mutated, refusing a line
// that no longer reads as the diff said.
func applyDefect(content string, d seededDefect) (string, error) {
	lines := strings.SplitAfter(content, "\n")
	end := max(d.end, d.Line)
	if d.Line < 1 || end > len(lines) || strings.TrimSuffix(lines[d.Line-1], "\n") != d.Original {
		return "", fmt.Errorf("%s:%d no longer reads %q", d.Path, d.Line, d.Original)
	}
	var repl []string
	if d.Mutated != "" {
		repl = []string{d.Mutated + lines[d.Line-1][len(d.Original):]}
	}
	return strings.Join(slices.Concat(lines[:d.Line-1], repl, lines[end:]), ""), nil
}

// reseedFiles swaps path's patch in the PR's files artifact (GitHub's list) for the seeded one and recounts it.
func reseedFiles(files, path, patch string) string {
	var list []map[string]any
	if json.Unmarshal([]byte(files), &list) != nil {
		return files
	}
	for _, f := range list {
		if f["filename"] != path {
			continue
		}
		adds, dels := 0, 0
		for _, l := range strings.Split(patch, "\n") {
			switch {
			case strings.HasPrefix(l, "+"):
				adds++
			case strings.HasPrefix(l, "-"):
				dels++
			}
		}
		f["patch"], f["additions"], f["deletions"], f["changes"] = patch, adds, dels, adds+dels
	}
	b, err := json.Marshal(list)
	if err != nil {
		return files
	}
	return string(b)
}

// remarshal decodes src (an item's decoded JSON) into dst.
func remarshal(src, dst any) error {
	b, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

// FormatSeedSummary renders the seed command's item table.
func FormatSeedSummary(items []SeedItem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-40s %-40s %s\n", "SOURCE ITEM", "SEEDED ITEM", "DEFECT")
	seeded := 0
	for _, it := range items {
		what := fmt.Sprintf("%s at %s:%d", it.Defect.Operator, it.Defect.Path, it.Defect.Line)
		if it.Skipped != "" {
			what = "skipped: " + it.Skipped
		} else {
			seeded++
		}
		fmt.Fprintf(&b, "%-40s %-40s %s\n", it.SourceID, it.ItemID, what)
	}
	fmt.Fprintf(&b, "%d item(s) seeded, %d skipped\n", seeded, len(items)-seeded)
	return b.String()
}
