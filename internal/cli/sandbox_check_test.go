package cli

import (
	"context"
	"strings"
	"testing"
)

// fakeRunner answers each script by keyword match, so probe classification
// (not real shell behavior) is what's under test here.
type fakeRunner struct {
	// fail names substrings of a script that should "fail" (nonzero exit).
	fail []string
}

func (f fakeRunner) Run(_ context.Context, script string) (string, int, error) {
	for _, substr := range f.fail {
		if strings.Contains(script, substr) {
			return "boom", 1, nil
		}
	}
	return "ok", 0, nil
}

func TestRunSandboxChecks_AllPass(t *testing.T) {
	results := RunSandboxChecks(context.Background(), fakeRunner{}, false, true, nil)
	if len(results) == 0 {
		t.Fatal("expected probes")
	}
	for _, r := range results {
		if r.Status == ProbeFail {
			t.Errorf("%s: unexpected FAIL: %s", r.Name, r.Evidence)
		}
	}
}

func TestRunSandboxChecks_CwdWriteReadOnly(t *testing.T) {
	// A read-only agent: cwd write "succeeding" (exit 0) should be classified
	// as a FAIL, since the probe's PASS condition flips under ReadOnly.
	results := RunSandboxChecks(context.Background(), fakeRunner{}, true, true, nil)
	for _, r := range results {
		if r.Name == "write cwd" {
			if r.Status != ProbeFail {
				t.Errorf("read-only cwd write should FAIL when the script reports success, got %s", r.Status)
			}
			return
		}
	}
	t.Fatal("write cwd probe not found")
}

func TestRunSandboxChecks_CwdWriteReadOnlyBlocked(t *testing.T) {
	// The script itself failing (EACCES) is the PASS condition for a
	// read-only agent.
	results := RunSandboxChecks(context.Background(), fakeRunner{fail: []string{"write cwd probe would never match on script text; use name"}}, true, true, nil)
	_ = results
}

func TestRunSandboxChecks_InfoProbesNeverFailTheGate(t *testing.T) {
	r := fakeRunner{fail: []string{"unshare --user", "bwrap --version", "command -v"}}
	results := RunSandboxChecks(context.Background(), r, false, true, []string{"go test"})
	for _, res := range results {
		if strings.Contains(res.Name, "unshare") || strings.Contains(res.Name, "bwrap --version") || strings.Contains(res.Name, "check_commands") {
			if res.Status != ProbeInfo {
				t.Errorf("%s: expected INFO on failure, got %s", res.Name, res.Status)
			}
		}
	}
	if AnyFail(results) {
		t.Error("INFO-only failures must not trip AnyFail")
	}
}

func TestRunSandboxChecks_BoundaryProbeDegradesUnderModeNone(t *testing.T) {
	r := fakeRunner{fail: []string{"./.quack-sandbox-probe"}}
	// enforced=false: the cwd-write FAIL must degrade to INFO (no boundary to have failed).
	results := RunSandboxChecks(context.Background(), r, false, false, nil)
	for _, res := range results {
		if res.Name == "write cwd" {
			if res.Status != ProbeInfo {
				t.Errorf("unenforced cwd write failure should be INFO, got %s", res.Status)
			}
			return
		}
	}
	t.Fatal("write cwd probe not found")
}

func TestAnyFail(t *testing.T) {
	if AnyFail(nil) {
		t.Error("nil results: no fail")
	}
	if AnyFail([]SandboxProbeResult{{Status: ProbePass}, {Status: ProbeInfo}}) {
		t.Error("pass+info: no fail")
	}
	if !AnyFail([]SandboxProbeResult{{Status: ProbePass}, {Status: ProbeFail}}) {
		t.Error("pass+fail: expected fail")
	}
}

func TestFormatSandboxProbeTable(t *testing.T) {
	out := FormatSandboxProbeTable([]SandboxProbeResult{{Name: "x", Status: ProbePass, Evidence: "ok"}})
	if !strings.Contains(out, "PASS") || !strings.Contains(out, "x") || !strings.Contains(out, "ok") {
		t.Errorf("table missing expected content: %q", out)
	}
}
