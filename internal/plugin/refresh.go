package plugin

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Revision is one plugin root and the commit its tree is at.
type Revision struct {
	Name string
	Path string
	Ref  string // pinned in the manifest: a commit, or a branch to float on
	Head string // the commit actually on disk
}

// refreshTimeout bounds startup: a hung remote must not block the server.
const refreshTimeout = 60 * time.Second

// Refresh brings each plugin tree named in manifestPath to its pinned ref and
// reports what is actually on disk. It never fails startup: a plugin that
// cannot be fetched keeps whatever tree it already has (or is reported with an
// empty Head), because losing the network must not cost the server its skills.
func Refresh(manifestPath, script string) []Revision {
	entries, err := parseManifest(manifestPath)
	if err != nil {
		slog.Warn("plugin manifest unreadable; skipping refresh", "component", "plugin", "manifest", manifestPath, "err", err)
		return nil
	}

	if script != "" {
		if _, err := os.Stat(script); err == nil {
			runFetch(script)
		} else {
			slog.Debug("plugin fetch script absent; reporting on-disk revisions only", "component", "plugin", "script", script)
		}
	}

	revs := make([]Revision, 0, len(entries))
	for _, e := range entries {
		r := Revision{Name: e.name, Path: e.path, Ref: e.ref, Head: onDiskRef(e.path)}
		switch {
		case r.Head == "":
			slog.Warn("plugin tree missing on disk", "component", "plugin", "name", r.Name, "path", r.Path, "pinned", r.Ref)
		case r.Head != r.Ref:
			// Expected when ref is a branch; a stale pin otherwise.
			slog.Info("plugin revision differs from pin", "component", "plugin", "name", r.Name, "pinned", r.Ref, "head", r.Head)
		default:
			slog.Info("plugin at pinned revision", "component", "plugin", "name", r.Name, "revision", r.Head)
		}
		revs = append(revs, r)
	}
	return revs
}

func runFetch(script string) {
	cmd := exec.Command("bash", script)
	cmd.Dir = filepath.Dir(filepath.Dir(script))
	done := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		slog.Warn("plugin fetch did not start; using trees already on disk", "component", "plugin", "err", err)
		return
	}
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			slog.Warn("plugin fetch failed; using trees already on disk", "component", "plugin", "err", err)
		}
	case <-time.After(refreshTimeout):
		_ = cmd.Process.Kill()
		slog.Warn("plugin fetch timed out; using trees already on disk", "component", "plugin", "timeout", refreshTimeout)
	}
}

// onDiskRef reads the stamp scripts/plugins.sh writes next to a fetched tree.
func onDiskRef(path string) string {
	b, err := os.ReadFile(filepath.Join(path, ".plugin-ref"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

type manifestEntry struct{ name, url, ref, path string }

// parseManifest reads the same fixed shape scripts/plugins.sh does. Keeping a
// hand parser here avoids a yaml dependency for four scalar keys.
// trimInlineComment drops a trailing YAML comment, which only starts after
// whitespace - so a "#" inside a value (a URL fragment) survives. Without it a
// pin annotated `ref: <sha>   # v4.9.0` never equals the on-disk head.
func trimInlineComment(v string) string {
	for i := 1; i < len(v); i++ {
		if v[i] == '#' && (v[i-1] == ' ' || v[i-1] == '\t') {
			return strings.TrimSpace(v[:i])
		}
	}
	return strings.TrimSpace(v)
}

func parseManifest(path string) ([]manifestEntry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []manifestEntry
	var cur *manifestEntry
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		val = trimInlineComment(val)
		switch key {
		case "- name":
			if cur != nil {
				out = append(out, *cur)
			}
			cur = &manifestEntry{name: val}
		case "url":
			if cur != nil {
				cur.url = val
			}
		case "ref":
			if cur != nil {
				cur.ref = val
			}
		case "path":
			if cur != nil {
				cur.path = val
			}
		}
	}
	if cur != nil {
		out = append(out, *cur)
	}
	for _, e := range out {
		if e.name == "" || e.url == "" || e.ref == "" || e.path == "" {
			return out, fmt.Errorf("incomplete entry %q", e.name)
		}
	}
	return out, nil
}

// Summary renders revisions for a single log line / run stamp, e.g.
// "dotagents@8205969,ponytail@adad50d".
func Summary(revs []Revision) string {
	parts := make([]string, 0, len(revs))
	for _, r := range revs {
		head := r.Head
		if head == "" {
			head = "missing"
		} else if len(head) > 7 {
			head = head[:7]
		}
		parts = append(parts, r.Name+"@"+head)
	}
	return strings.Join(parts, ",")
}
