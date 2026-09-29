package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestSplitFiles: file grants get file rules; dirs and missing paths keep dir rules.
func TestSplitFiles(t *testing.T) {
	dir := t.TempDir()
	file, missing := filepath.Join(dir, "f"), filepath.Join(dir, "gone")
	writeFile(t, file, "")
	dirs, files := splitFiles([]string{dir, file, missing})
	if !slices.Equal(dirs, []string{dir, missing}) || !slices.Equal(files, []string{file}) {
		t.Errorf("dirs %v files %v", dirs, files)
	}
}

// TestSandboxExecClaimsGrantFDs: the handle behind a /proc/self/fd grant is closed before the target runs, and a
// grant naming no open handle is refused rather than skipped.
func TestSandboxExecClaimsGrantFDs(t *testing.T) {
	requireLandlock(t)
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f, err := openNoFollow(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	args := append([]string{SandboxExecArg, "--rw", "/proc/self/fd/3", "--ro", "/proc"}, landlockBaseRO...)
	cmd := exec.Command(self, append(args, "--", "sh", "-c", "[ ! -e /proc/self/fd/3 ]")...)
	cmd.ExtraFiles = []*os.File{f}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("target inherited the grant's handle (fd 3): %v %s", err, out)
	}
	out, code := runSandboxExec(t, append([]string{"--rw", "/proc/self/fd/9"}, append(landlockBaseRO, "--", "true")...)...)
	if code == 0 || !strings.Contains(out, "no open handle") {
		t.Errorf("unresolvable fd grant: exit=%d %q, want a refusal", code, out)
	}
}
