package workspace

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestRunArgvNamesFileSizeLimit (#798): a child killed by a limit QUACK set must
// say so. Before this, SIGXFSZ surfaced as the command's own opaque failure -
// the ACP regression cost four bisect cycles precisely because the child's
// error never mentioned the ceiling that killed it.
func TestRunArgvNamesFileSizeLimit(t *testing.T) {
	if _, err := os.Stat("/usr/bin/prlimit"); err != nil {
		t.Skipf("SKIPPING: prlimit(1) not installed (%v)", err)
	}
	dir := t.TempDir()
	caps := DefaultCaps()
	caps.Sandbox = SandboxNone
	caps.Limits = Limits{FileSizeMB: 1}

	// dd past the 1MB ceiling; prlimit's SIGXFSZ lands on the child.
	res, err := RunArgv(context.Background(), dir, []string{"dd", "if=/dev/zero", "of=big.bin", "bs=1M", "count=8"}, caps)
	if err != nil {
		t.Fatalf("RunArgv: %v", err)
	}
	if res.ExitCode == 0 {
		t.Skip("SKIPPING: the child was not limited here (no prlimit in this sandbox mode)")
	}
	if !strings.Contains(res.Output, "max_file_size_mb") {
		t.Errorf("output does not name the limit that killed it; got:\n%s", res.Output)
	}
}

// TestFileSizeLimitNoteSilentWhenUnset: no limit configured, nothing to blame.
func TestFileSizeLimitNoteSilentWhenUnset(t *testing.T) {
	if got := fileSizeLimitNote(nil, Limits{FileSizeMB: 0}); got != "" {
		t.Errorf("fileSizeLimitNote(nil, unset) = %q, want empty", got)
	}
}
