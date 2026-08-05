package tools

import (
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/workspace"
)

const defaultReadLimit = 2000

// binarySniffBytes: NUL-byte detection window for binary check.
const binarySniffBytes = 8 * 1024

// Bound bytes (not just count) - minified lines can be MBs.
const (
	grepMatchMaxChars = 400
	grepTotalMaxBytes = 256 * 1024
	grepFileMaxBytes  = 4 * 1024 * 1024
)

// truncateMiddle: middle-out truncation, value is at edges.
func truncateMiddle(s string, maxChars int) string {
	const marker = "…[elided]…"
	if len(s) <= maxChars || maxChars <= len(marker) {
		return s
	}
	keep := maxChars - len(marker)
	head := keep / 2
	return strings.ToValidUTF8(s[:head], "") + marker + strings.ToValidUTF8(s[len(s)-(keep-head):], "")
}

// fsBinding: (userID, jail, caps) triple closed over at construction for every fs tool.
type fsBinding struct {
	userID string
	jail   *workspace.Jail
	caps   workspace.Caps
	cwd     string
	chatID  string
	nodeDir string
}

// withCwd: returns copy bound to this call's context (chat scope, node dir, cwd).
func (b fsBinding) withCwd(ctx agent.Context) fsBinding {
	b.chatID, b.nodeDir = scopeFromContext(ctx)
	b.cwd = cwdFromState(ctx)
	return b
}

// resolve: cwd-, node- and chat-aware Jail.Resolve for fs tools.
func (b fsBinding) resolve(p string) (string, error) {
	return b.jail.Resolve(b.userID, b.chatID, jailPath(b.nodeDir, b.cwd, p))
}

// newFSBinding: resolves Deps into fsBinding. Deps.Workspace nil is an error.
func newFSBinding(d Deps) (fsBinding, error) {
	if d.Workspace == nil {
		return fsBinding{}, fmt.Errorf("tools: filesystem tools require workspace to be configured (see workspace.root in quack.yaml)")
	}
	userID := d.WorkspaceUserID
	if userID == "" {
		return fsBinding{}, fmt.Errorf("tools: filesystem tools require a WorkspaceUserID")
	}
	caps := d.WorkspaceCaps
	if caps.IsZero() {
		caps = workspace.DefaultCaps()
	}
	return fsBinding{userID: userID, jail: d.Workspace, caps: caps}, nil
}

// isBinary: reports whether head bytes contain a NUL.
func isBinary(b []byte) bool {
	return bytes.IndexByte(b, 0) >= 0
}

// readCapped: reads up to max+1 bytes, reports if capped.
func readCapped(r io.Reader, max int64) (data []byte, capped bool, err error) {
	data, err = io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(data)) > max {
		return data[:max], true, nil
	}
	return data, false, nil
}

// read_file

type readFileArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"` // 0-based line
	Limit  int    `json:"limit,omitempty"`  // lines; default 500
}

type readFileResult struct {
	Content    string `json:"content"`
	Truncated  bool   `json:"truncated"`
	TotalLines int    `json:"total_lines"`
	NextOffset int `json:"next_offset,omitempty"`
}

func newReadFile(d Deps) (tool.Tool, error) {
	b, err := newFSBinding(d)
	if err != nil {
		return nil, err
	}
	return functiontool.New[readFileArgs, readFileResult](
		functiontool.Config{
			Name: "read_file",
			Description: fmt.Sprintf("Read a text file from your workspace. `path` is workspace-relative (never "+
				"absolute). `offset` (0-based line, default 0) and `limit` (lines, default %d) window a large "+
				"file; the result reports `total_lines` and sets `truncated: true` when more content exists than "+
				"was returned. When truncated, the result gives `next_offset` - call again with `offset: next_offset` "+
				"to continue (or read the file's END with `offset: total_lines - limit`). Never an error. Binary files are rejected.",
				defaultReadLimit),
		},
		func(ctx agent.Context, a readFileArgs) (readFileResult, error) { return b.withCwd(ctx).readFile(a) },
	)
}

// readFile: binary detection on head, byte-capped read, offset/limit windowing.
func (b fsBinding) readFile(a readFileArgs) (readFileResult, error) {
	real, err := b.resolve(a.Path)
	if err != nil {
		return readFileResult{}, err
	}
	info, err := os.Stat(real)
	if err != nil {
		return readFileResult{}, fmt.Errorf("read_file: %w", err)
	}
	if info.IsDir() {
		return readFileResult{}, fmt.Errorf("read_file: %q is a directory", a.Path)
	}
	f, err := os.Open(real)
	if err != nil {
		return readFileResult{}, fmt.Errorf("read_file: %w", err)
	}
	defer f.Close()

	sniff := make([]byte, binarySniffBytes)
	n, _ := io.ReadFull(f, sniff)
	if isBinary(sniff[:n]) {
		return readFileResult{}, fmt.Errorf("read_file: %q is a binary file", a.Path)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return readFileResult{}, fmt.Errorf("read_file: %w", err)
	}

	data, byteCapped, err := readCapped(f, b.caps.MaxReadBytes)
	if err != nil {
		return readFileResult{}, fmt.Errorf("read_file: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	total := len(lines)

	limit := a.Limit
	if limit <= 0 {
		limit = defaultReadLimit
	}
	offset := a.Offset
	if offset < 0 {
		offset = 0
	}

	var window []string
	truncated := byteCapped
	nextOffset := 0
	if offset < total {
		end := offset + limit
		if end >= total {
			end = total
		} else {
			truncated = true
			nextOffset = end
		}
		window = lines[offset:end]
	}
	return readFileResult{
		Content:    strings.Join(window, "\n"),
		Truncated:  truncated,
		TotalLines: total,
		NextOffset: nextOffset,
	}, nil
}

// write_file / edit_file / list_dir

type listDirArgs struct {
	Path  string `json:"path,omitempty"`
	Depth int    `json:"depth,omitempty"` // default 2
}

type dirEntry struct {
	Path string `json:"path"`
	Dir  bool   `json:"dir"`
	Size int64  `json:"size"`
}

type listDirResult struct {
	Entries   []dirEntry `json:"entries"`
	Truncated bool       `json:"truncated"`
	Cwd string `json:"cwd"`
}

func newListDir(d Deps) (tool.Tool, error) {
	b, err := newFSBinding(d)
	if err != nil {
		return nil, err
	}
	return functiontool.New[listDirArgs, listDirResult](
		functiontool.Config{
			Name: "list_dir",
			Description: fmt.Sprintf("List files and directories in your workspace. `path` is workspace-relative "+
				"(default: workspace root). `depth` bounds how many levels are listed (default 2). Returned "+
				"paths are workspace-relative. Caps at %d entries; `truncated: true` means more exist - narrow "+
				"`path`, or use `glob`/`grep` instead.", b.caps.MaxListEntries),
		},
		func(ctx agent.Context, a listDirArgs) (listDirResult, error) { return b.withCwd(ctx).listDir(a) },
	)
}

// listDir: depth-bounded walk under path, returning cwd-relative entries.
func (b fsBinding) listDir(a listDirArgs) (listDirResult, error) {
	base, err := b.resolve(a.Path)
	if err != nil {
		return listDirResult{}, err
	}
	info, err := os.Stat(base)
	if err != nil {
		return listDirResult{}, fmt.Errorf("list_dir: %w", err)
	}
	if !info.IsDir() {
		return listDirResult{}, fmt.Errorf("list_dir: %q is not a directory", a.Path)
	}
	relRoot, err := b.resolve("")
	if err != nil {
		return listDirResult{}, err
	}
	depth := a.Depth
	if depth <= 0 {
		depth = 2
	}

	var entries []dirEntry
	truncated := false
	walkErr := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == base {
			return nil
		}
		rel, rerr := filepath.Rel(base, p)
		if rerr != nil {
			return rerr
		}
		level := len(strings.Split(rel, string(filepath.Separator)))
		if level > depth {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if len(entries) >= b.caps.MaxListEntries {
			truncated = true
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		relToRoot, rerr := filepath.Rel(relRoot, p)
		if rerr != nil {
			return rerr
		}
		var size int64
		if !d.IsDir() {
			if fi, ferr := d.Info(); ferr == nil {
				size = fi.Size()
			}
		}
		entries = append(entries, dirEntry{Path: filepath.ToSlash(relToRoot), Dir: d.IsDir(), Size: size})
		return nil
	})
	if walkErr != nil {
		return listDirResult{}, fmt.Errorf("list_dir: %w", walkErr)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return listDirResult{Entries: entries, Truncated: truncated, Cwd: displayCwd(b.cwd)}, nil
}

// glob

type globArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
}

type globResult struct {
	Paths     []string `json:"paths"`
	Truncated bool     `json:"truncated"`
	Cwd string `json:"cwd"`
}

func newGlob(d Deps) (tool.Tool, error) {
	b, err := newFSBinding(d)
	if err != nil {
		return nil, err
	}
	return functiontool.New[globArgs, globResult](
		functiontool.Config{
			Name: "glob",
			Description: fmt.Sprintf("Find files by name pattern (doublestar glob syntax, e.g. `**/*.go` or "+
				"`src/**/*.test.ts`). `path` scopes the search (default: workspace root). Returned paths are "+
				"workspace-relative. Caps at %d results; `truncated: true` means more exist - narrow the "+
				"pattern.", b.caps.MaxResults),
		},
		func(ctx agent.Context, a globArgs) (globResult, error) { return b.withCwd(ctx).glob(a) },
	)
}

// glob: doublestar matching rooted at path, workspace-relative results.
func (b fsBinding) glob(a globArgs) (globResult, error) {
	if strings.TrimSpace(a.Pattern) == "" {
		return globResult{}, fmt.Errorf("glob: pattern is empty")
	}
	base, err := b.resolve(a.Path)
	if err != nil {
		return globResult{}, err
	}
	if info, serr := os.Stat(base); serr != nil || !info.IsDir() {
		return globResult{}, fmt.Errorf("glob: %q is not a directory", a.Path)
	}
	matches, err := doublestar.Glob(os.DirFS(base), a.Pattern)
	if err != nil {
		return globResult{}, fmt.Errorf("glob: %w", err)
	}
	// Drop hits inside vendored/generated trees unless path targets one.
	if !pathHasGeneratedDir(a.Path) {
		kept := matches[:0]
		for _, m := range matches {
			if !pathHasGeneratedDir(m) {
				kept = append(kept, m)
			}
		}
		matches = kept
	}
	sort.Strings(matches)
	truncated := false
	if len(matches) > b.caps.MaxResults {
		matches = matches[:b.caps.MaxResults]
		truncated = true
	}
	// Re-root results under path for direct reuse.
	paths := make([]string, len(matches))
	for i, m := range matches {
		paths[i] = filepath.ToSlash(filepath.Join(a.Path, m))
	}
	return globResult{Paths: paths, Truncated: truncated, Cwd: displayCwd(b.cwd)}, nil
}

// pathHasGeneratedDir: checks if any path component is in workspace.SkipDir.
func pathHasGeneratedDir(p string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if seg != "" && seg != "." && workspace.SkipDir(seg) {
			return true
		}
	}
	return false
}

// grep

type grepArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
	Glob    string `json:"glob,omitempty"`    // filter, e.g. *.go (matched against the basename)
	Context int    `json:"context,omitempty"` // lines of context; default 0
}

type grepMatch struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

type grepResult struct {
	Matches   []grepMatch `json:"matches"`
	Truncated bool        `json:"truncated"`
	Cwd string `json:"cwd"`
}

func newGrep(d Deps) (tool.Tool, error) {
	b, err := newFSBinding(d)
	if err != nil {
		return nil, err
	}
	return functiontool.New[grepArgs, grepResult](
		functiontool.Config{
			Name: "grep",
			Description: fmt.Sprintf("Search file contents by regular expression (Go/RE2 syntax) across your "+
				"workspace. `path` scopes the search (default: workspace root); `glob` filters which files are "+
				"searched by basename (e.g. `*.go`); `context` adds N surrounding lines to each match's `text`. "+
				"Binary files are skipped. Caps at %d matches; `truncated: true` means more exist - narrow the "+
				"pattern or path.", b.caps.MaxResults),
		},
		func(ctx agent.Context, a grepArgs) (grepResult, error) { return b.withCwd(ctx).grep(a) },
	)
}

// grep: regexp scan of non-binary files under path, capped at MaxResults.
func (b fsBinding) grep(a grepArgs) (grepResult, error) {
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return grepResult{}, fmt.Errorf("grep: invalid pattern: %w", err)
	}
	base, err := b.resolve(a.Path)
	if err != nil {
		return grepResult{}, err
	}
	relRoot, err := b.resolve("")
	if err != nil {
		return grepResult{}, err
	}
	ctxLines := a.Context
	if ctxLines < 0 {
		ctxLines = 0
	}

	var matches []grepMatch
	truncated := false
	totalBytes := 0
	walkErr := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if truncated {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
		// Never descend into vendored/generated trees unless path targets one.
			if p != base && workspace.SkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if a.Glob != "" {
			if ok, _ := filepath.Match(a.Glob, d.Name()); !ok {
				return nil
			}
		}
		fileMatches, ferr := grepFile(p, re, ctxLines)
		if ferr != nil {
			return nil // unreadable, binary, or too large: skip silently
		}
		relToRoot, rerr := filepath.Rel(relRoot, p)
		if rerr != nil {
			return rerr
		}
		for _, fm := range fileMatches {
			if len(matches) >= b.caps.MaxResults || totalBytes >= grepTotalMaxBytes {
				truncated = true
				break
			}
			// Cap per-match text too; a "line" can be megabytes.
			fm.Text = truncateMiddle(fm.Text, grepMatchMaxChars)
			fm.Path = filepath.ToSlash(relToRoot)
			totalBytes += len(fm.Text)
			matches = append(matches, fm)
		}
		return nil
	})
	if walkErr != nil {
		return grepResult{}, fmt.Errorf("grep: %w", walkErr)
	}
	return grepResult{Matches: matches, Truncated: truncated, Cwd: displayCwd(b.cwd)}, nil
}

// grepFile: scans one file for matching lines. Binary or too-large files are skipped.
func grepFile(path string, re *regexp.Regexp, ctxLines int) ([]grepMatch, error) {
	// Check size before reading (OOM guard for vendored bundles).
	if st, serr := os.Stat(path); serr == nil && st.Size() > grepFileMaxBytes {
		return nil, fmt.Errorf("grep: %q is larger than the %d-byte scan limit", path, int64(grepFileMaxBytes))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if isBinary(data[:min(len(data), binarySniffBytes)]) {
		return nil, fmt.Errorf("grep: %q is a binary file", path)
	}
	lines := strings.Split(string(data), "\n")
	var out []grepMatch
	for i, ln := range lines {
		if !re.MatchString(ln) {
			continue
		}
		text := ln
		if ctxLines > 0 {
			start := max(0, i-ctxLines)
			end := min(len(lines)-1, i+ctxLines)
			text = strings.Join(lines[start:end+1], "\n")
		}
		out = append(out, grepMatch{Line: i + 1, Text: text})
	}
	return out, nil
}

// delete_path

func logDeletePath(userID, path string, recursive bool, deleted int) {
	slog.Info("workspace mutation", "component", "tools", "tool", "delete_path",
		"user", userID, "path", path, "recursive", recursive, "deleted", deleted)
}
