package workspace

import (
	"os"
	"testing"
)

// TestMain mirrors main()'s __sandbox-exec dispatch (RunSandboxExecIfInvoked,
// wired in cmd/quack/main.go) so this package's own tests can self-spawn the
// REAL shim: os.Executable() resolves to this test binary, and without this
// hook it would have no idea what to do with a __sandbox-exec argv (see
// cmd/quack/git_askpass_test.go's TestMain for the identical pattern).
func TestMain(m *testing.M) {
	RunSandboxExecIfInvoked()
	os.Exit(m.Run())
}
