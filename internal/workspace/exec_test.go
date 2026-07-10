package workspace

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestContainsShellMetachar(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"go test ./...", false},
		{"echo hi", false},
		{"echo hi | grep x", true},
		{"echo hi; rm -rf /", true},
		{"echo $HOME", true},
		{"echo `whoami`", true},
		{"cmd < in", true},
		{"cmd > out", true},
		{"echo hi && echo bye", true},
		{"(echo hi)", true},
	}
	for _, c := range cases {
		if got := ContainsShellMetachar(c.s); got != c.want {
			t.Errorf("ContainsShellMetachar(%q) = %v, want %v", c.s, got, c.want)
		}
	}
}

func TestSplitArgv(t *testing.T) {
	cases := []struct {
		s    string
		want []string
	}{
		{"go test ./...", []string{"go", "test", "./..."}},
		{`git commit -m "a message with spaces"`, []string{"git", "commit", "-m", "a message with spaces"}},
		{"echo 'single quoted'", []string{"echo", "single quoted"}},
		{"  leading  spaces  ", []string{"leading", "spaces"}},
	}
	for _, c := range cases {
		got, err := SplitArgv(c.s)
		if err != nil {
			t.Fatalf("SplitArgv(%q): %v", c.s, err)
		}
		if len(got) != len(c.want) {
			t.Fatalf("SplitArgv(%q) = %v, want %v", c.s, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("SplitArgv(%q)[%d] = %q, want %q", c.s, i, got[i], c.want[i])
			}
		}
	}
}

func TestSplitArgvErrors(t *testing.T) {
	for _, s := range []string{"", "   ", `unterminated "quote`, `trailing\`} {
		if _, err := SplitArgv(s); err == nil {
			t.Errorf("SplitArgv(%q): want error", s)
		}
	}
}

func TestRunArgvSuccess(t *testing.T) {
	res, err := RunArgv(context.Background(), t.TempDir(), []string{"echo", "hello"}, DefaultCaps())
	if err != nil {
		t.Fatalf("RunArgv: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if !strings.Contains(res.Output, "hello") {
		t.Errorf("Output = %q, want to contain 'hello'", res.Output)
	}
}

func TestRunArgvNonZeroExitIsNotAnError(t *testing.T) {
	res, err := RunArgv(context.Background(), t.TempDir(), []string{"false"}, DefaultCaps())
	if err != nil {
		t.Fatalf("RunArgv: unexpected error for a plain non-zero exit: %v", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("ExitCode = 0, want non-zero")
	}
}

func TestRunArgvMissingBinaryErrors(t *testing.T) {
	_, err := RunArgv(context.Background(), t.TempDir(), []string{"this-binary-does-not-exist-xyz"}, DefaultCaps())
	if err == nil {
		t.Fatal("RunArgv: want error for a missing binary")
	}
}

func TestRunArgvCwdIsPinned(t *testing.T) {
	dir := t.TempDir()
	res, err := RunArgv(context.Background(), dir, []string{"pwd"}, DefaultCaps())
	if err != nil {
		t.Fatalf("RunArgv: %v", err)
	}
	if !strings.Contains(strings.TrimSpace(res.Output), dir) {
		t.Errorf("pwd output = %q, want to contain jailed dir %q", res.Output, dir)
	}
}

func TestRunArgvTimeout(t *testing.T) {
	caps := DefaultCaps()
	caps.Timeout = 50 * time.Millisecond
	res, err := RunArgv(context.Background(), t.TempDir(), []string{"sleep", "5"}, caps)
	if err == nil {
		t.Fatal("RunArgv: want a timeout error")
	}
	if !res.TimedOut {
		t.Error("ExecResult.TimedOut = false, want true")
	}
}

func TestRunArgvOutputCap(t *testing.T) {
	caps := DefaultCaps()
	caps.MaxOutputBytes = 10
	res, err := RunArgv(context.Background(), t.TempDir(), []string{"echo", "this is a much longer line than the cap"}, caps)
	if err != nil {
		t.Fatalf("RunArgv: %v", err)
	}
	if !strings.Contains(res.Output, "truncated") {
		t.Errorf("Output = %q, want a truncation marker", res.Output)
	}
	if int64(len(res.Output)) > caps.MaxOutputBytes+int64(len("... (truncated)\n")) {
		t.Errorf("Output length %d exceeds the cap plus marker", len(res.Output))
	}
}
