package pluginreg

import (
	"context"
	"fmt"
	"strings"
	"testing/fstest"
)

// TreeAt builds an in-memory fs.FS snapshot of subdir as it existed at sha -
// replay reads recorded skill text through this instead of the live clone.
// Fails, naming the plugin and sha, if the clone is missing or unreachable.
func TreeAt(ctx context.Context, root, name, sha, subdir string) (fstest.MapFS, error) {
	dir := CloneDir(root, name)
	// subdir is trusted off a row read from disk without re-parsing, same as
	// Plugin.Root - an escaping one falls back to "skills" at the clone
	// root instead, so replay never lists outside the plugin's own tree.
	if joined, err := containedPath(".", subdir); err == nil {
		subdir = joined
	} else {
		subdir = "skills"
	}
	// -z/NUL-split (core.quotePath would else C-quote a non-ASCII path and
	// break the `show` below); ":(literal)" pins subdir against pathspec magic.
	// ponytail: one `show` per file; `git archive` if a plugin ships hundreds.
	out, err := gitOutput(ctx, dir, "ls-tree", "-r", "-z", "--name-only", sha, "--", ":(literal)"+subdir)
	if err != nil {
		return nil, fmt.Errorf("plugin %q@%s: list tree: %w", name, sha, err)
	}
	mfs := fstest.MapFS{}
	for _, p := range strings.Split(strings.TrimSuffix(out, "\x00"), "\x00") {
		if p == "" {
			continue
		}
		rel := strings.TrimPrefix(p, subdir+"/")
		data, err := gitOutput(ctx, dir, "show", sha+":"+p)
		if err != nil {
			return nil, fmt.Errorf("plugin %q@%s: show %s: %w", name, sha, p, err)
		}
		mfs[rel] = &fstest.MapFile{Data: []byte(data)}
	}
	return mfs, nil
}
