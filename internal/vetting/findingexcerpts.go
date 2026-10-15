// Quack-fetched source excerpts: the lines a staged review cites, read from the reviewed clone so the
// judge checks each claim without spending a model call per read_file.
package vetting

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	excerptContext   = 6       // lines shown either side of a cite
	excerptMaxSpan   = 40      // a wider range cite shows only its first lines
	excerptMaxCount  = 20      // cites rendered per round
	excerptBudget    = 16_000  // bytes across all rendered cites (~4k tokens)
	excerptLineChars = 200     // a longer source line is cut
	excerptMaxFile   = 2 << 20 // a larger file gets a note, not an excerpt
)

// citeRe: path:line or path:start-end in prose; the path needs an extension, so "step 2:3" never matches.
var citeRe = regexp.MustCompile(`([\w./-]+\.\w+):(\d+)(?:-(\d+))?`)

// sourceExcerptsHeader labels the section as quack's own read of the tree, not the reviewer's words.
const sourceExcerptsHeader = "QUACK-FETCHED SOURCE EXCERPTS - quack read each path:line the review cites from the reviewed tree " +
	"(6 lines either side, cited lines marked >). This is the code itself, not the reviewer's account of it: check claims " +
	"against these lines instead of re-reading them, and open files only for context they lack. A note in place of an " +
	"excerpt (file not found, line past the end) is itself evidence about the claim.\n\n"

// sourceCite: one cited span. Anchored cites (a finding's own path/line) note a missing file; prose cites
// skip it, since "retry.go:13" may name a file by its base name.
type sourceCite struct {
	path       string
	start, end int
	anchored   bool
}

// citeFile: one cited file's lines, or why it has none.
type citeFile struct {
	lines []string
	note  string
}

// reviewExcerptsSection renders the staged review's cites from the probe clone; n counts rendered cites.
func reviewExcerptsSection(cfg Config, act workerActivity) (string, int) {
	sd, ok := act.stagedDelivery["review"]
	if !ok {
		return "", 0
	}
	dir, ok := probeClone(cfg)
	if !ok {
		return "", 0
	}
	return sourceExcerptsSection(dir, reviewCites(sd, dir))
}

// reviewCites lists sd's cites without repeats: finding anchors first, then cites in verified items and finding bodies.
func reviewCites(sd StagedDelivery, dir string) []sourceCite {
	var cites []sourceCite
	seen := map[sourceCite]bool{}
	add := func(c sourceCite) {
		c.path, c.end = citePath(dir, c.path), max(c.start, c.end)
		key := c
		key.anchored = false
		if c.start > 0 && !seen[key] {
			seen[key] = true
			cites = append(cites, c)
		}
	}
	for _, c := range sd.Comments {
		add(sourceCite{path: c.Path, start: c.Line, anchored: true})
	}
	texts := append([]string{}, sd.Verified...)
	for _, c := range sd.Comments {
		texts = append(texts, c.Body)
	}
	for _, t := range texts {
		for _, m := range citeRe.FindAllStringSubmatch(t, -1) {
			start, _ := strconv.Atoi(m[2])
			end, _ := strconv.Atoi(m[3])
			add(sourceCite{path: m[1], start: start, end: end})
		}
	}
	return cites
}

// citePath makes p relative to dir; a path outside dir stays as given and os.Root refuses it.
func citePath(dir, p string) string {
	if filepath.IsAbs(p) {
		if rel, err := filepath.Rel(dir, p); err == nil {
			p = rel
		}
	}
	return filepath.Clean(p)
}

// sourceExcerptsSection renders cites from the tree at dir within excerptMaxCount and excerptBudget.
func sourceExcerptsSection(dir string, cites []sourceCite) (string, int) {
	if len(cites) == 0 {
		return "", 0
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", 0
	}
	defer root.Close()
	files := map[string]citeFile{}
	var sb strings.Builder
	n, skipped := 0, 0
	for _, c := range cites {
		block := renderCite(root, files, c)
		switch {
		case block == "":
		case n >= excerptMaxCount || sb.Len()+len(block) > excerptBudget:
			skipped++
		default:
			sb.WriteString(block)
			n++
		}
	}
	if n == 0 {
		return "", 0
	}
	if skipped > 0 {
		fmt.Fprintf(&sb, "(%d more cites not excerpted: excerpt budget reached)\n", skipped)
	}
	return sourceExcerptsHeader + sb.String() + "\n", n
}

// renderCite renders one cite's excerpt or note; "" for a prose cite of a file not in the tree.
func renderCite(root *os.Root, files map[string]citeFile, c sourceCite) string {
	f, ok := files[c.path]
	if !ok {
		f = readCiteFile(root, c.path)
		files[c.path] = f
	}
	loc := fmt.Sprintf("%s:%d", c.path, c.start)
	if c.end > c.start {
		loc = fmt.Sprintf("%s-%d", loc, c.end)
	}
	switch {
	case f.note != "" && !c.anchored:
		return ""
	case f.note != "":
		return fmt.Sprintf("%s - %s\n\n", loc, f.note)
	case c.start > len(f.lines):
		return fmt.Sprintf("%s - cited line %d is past the end of the file (%d lines)\n\n", loc, c.start, len(f.lines))
	}
	return loc + excerptBody(f.lines, c) + "\n"
}

// excerptBody numbers lines around c, marking the cited ones; c.start is within lines.
func excerptBody(lines []string, c sourceCite) string {
	last := min(c.end, len(lines), c.start+excerptMaxSpan-1)
	from, to := max(1, c.start-excerptContext), min(len(lines), last+excerptContext)
	var sb strings.Builder
	fmt.Fprintf(&sb, " (lines %d-%d of %d)\n", from, to, len(lines))
	for i := from; i <= to; i++ {
		mark := "  "
		if i >= c.start && i <= last {
			mark = "> "
		}
		fmt.Fprintf(&sb, "%s%5d  %s\n", mark, i, clipLine(lines[i-1]))
	}
	switch {
	case c.end > len(lines):
		fmt.Fprintf(&sb, "  (cited range ends at line %d, past the end of the file)\n", c.end)
	case c.end > last:
		fmt.Fprintf(&sb, "  (cited range clipped to its first %d lines)\n", excerptMaxSpan)
	}
	return sb.String()
}

// readCiteFile reads path under root, refusing large files and anything outside the tree.
func readCiteFile(root *os.Root, path string) citeFile {
	fi, err := root.Stat(path)
	switch {
	case err != nil:
		return citeFile{note: "file not found in the reviewed tree"}
	case fi.IsDir():
		return citeFile{note: "is a directory, not a file"}
	case fi.Size() > excerptMaxFile:
		return citeFile{note: fmt.Sprintf("file is %d bytes, too large to excerpt", fi.Size())}
	}
	data, err := root.ReadFile(path)
	if err != nil {
		return citeFile{note: "file unreadable: " + err.Error()}
	}
	if len(data) == 0 {
		return citeFile{}
	}
	return citeFile{lines: strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")}
}

// clipLine trims a carriage return and cuts a minified or generated line to excerptLineChars.
func clipLine(l string) string {
	l = strings.TrimSuffix(l, "\r")
	if len(l) <= excerptLineChars {
		return l
	}
	return strings.ToValidUTF8(l[:excerptLineChars], "") + "…"
}
