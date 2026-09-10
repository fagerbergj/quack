// Command quack-sandbox is the Landlock self-exec target
// (workspace.RunSandboxExecIfInvoked): a small static binary so a sandboxed
// child re-execs a few MB instead of the full server binary.
// landlockSelfExe prefers this binary when it sits beside the server; falls
// back to re-execing the server itself otherwise.
package main

import (
	"fmt"
	"os"

	"github.com/fagerbergj/quack/internal/workspace"
)

func main() {
	workspace.RunSandboxExecIfInvoked()
	fmt.Fprintln(os.Stderr, "quack-sandbox: only invoked as __sandbox-exec (see internal/workspace.RunSandboxExecIfInvoked)")
	os.Exit(1)
}
