package pluginreg

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing/fstest"
)

// VerifyCommit checks that sha is a commit reachable in name's clone under
// root - replay's LOAD-time refusal (#1427 P4): a missing clone or an
// unknown sha must fail before any round runs, not mid-run.
func VerifyCommit(ctx context.Context, root, name, sha string) error {
	dir := CloneDir(root, name)
	if _, err := os.Stat(dir); err != nil {
		return fmt.Errorf("plugin %q: no clone at %s: %w", name, dir, err)
	}
	if err := gitRun(ctx, dir, "cat-file", "-e", sha+"^{commit}"); err != nil {
		return fmt.Errorf("plugin %q: sha %s not found in the clone: %w", name, sha, err)
	}
	return nil
}

// TreeAt builds an in-memory fs.FS snapshot of subdir (a repo-relative path,
// "" for the repo root) as it existed at sha - replay reads recorded skill
// text through this instead of the live clone. subdir need not exist at sha
// (an empty tree, not an error).
//
// ponytail: one `git show` per file, fine for a skill tree's handful of
// files; switch to `git archive` if a plugin ships hundreds of files.
func TreeAt(ctx context.Context, root, name, sha, subdir string) (fstest.MapFS, error) {
	dir := CloneDir(root, name)
	args := []string{"ls-tree", "-r", "--name-only", sha}
	if subdir != "" {
		args = append(args, "--", subdir)
	}
	out, err := gitOutput(ctx, dir, args...)
	if err != nil {
		return nil, fmt.Errorf("plugin %q@%s: list tree: %w", name, sha, err)
	}
	mfs := fstest.MapFS{}
	for _, p := range strings.Split(strings.TrimSpace(out), "\n") {
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
