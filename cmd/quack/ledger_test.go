package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// buildRecovererOrWarn must never abort `ledger recover` on a build failure -
// it degrades to nil (every orphan reported Unresolved) and warns on stderr
// instead, since this is a diagnostics command and a misconfigured extension
// must not hide the orphans it might otherwise explain.
func TestBuildRecovererOrWarn_ConfigLoadFailureDegrades(t *testing.T) {
	t.Setenv("QUACK_CONFIG", t.TempDir()+"/does-not-exist.yaml")
	var errBuf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&errBuf)

	rec := buildRecovererOrWarn(cmd, false)
	if rec != nil {
		t.Fatalf("recoverer = %v, want nil on a config load failure", rec)
	}
	if !strings.Contains(errBuf.String(), "continuing without a recoverer") {
		t.Errorf("stderr = %q, want a warning that recovery continues without a recoverer", errBuf.String())
	}
}

func TestBuildRecovererOrWarn_DryRunSkipsBuild(t *testing.T) {
	// A bad QUACK_CONFIG would make a non-dry-run call warn; --dry-run must
	// never even attempt the build (and so never warn).
	t.Setenv("QUACK_CONFIG", t.TempDir()+"/does-not-exist.yaml")
	var errBuf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetErr(&errBuf)

	rec := buildRecovererOrWarn(cmd, true)
	if rec != nil {
		t.Fatalf("recoverer = %v, want nil under --dry-run", rec)
	}
	if errBuf.String() != "" {
		t.Errorf("stderr = %q, want no output under --dry-run", errBuf.String())
	}
}
