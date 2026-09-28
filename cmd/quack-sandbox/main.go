// Command quack-sandbox is the small __sandbox-exec/__reap self-exec target, so a sandboxed child
// re-execs a few MB instead of the server; landlockSelfExe prefers it when it sits beside the server.
package main

import (
	"fmt"
	"os"

	"github.com/fagerbergj/quack/internal/workspace"
)

func main() {
	workspace.RunSandboxExecIfInvoked()
	fmt.Fprintln(os.Stderr, "quack-sandbox: only invoked as __sandbox-exec or __reap (see internal/workspace.RunSandboxExecIfInvoked)")
	os.Exit(1)
}
