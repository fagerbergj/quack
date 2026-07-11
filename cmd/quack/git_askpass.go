package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/fagerbergj/quack/internal/tools"
)

// gitAskpassMain is quack's GIT_ASKPASS mode, entered via argv[0] dispatch
// (the busybox pattern): the git tools maintain a symlink
// <workspace root>/.quack-askpass -> the quack binary and set
// GIT_ASKPASS=<that symlink>; main() sees isGitAskpassInvocation() and lands
// here BEFORE cobra. This is required because git execs $GIT_ASKPASS
// DIRECTLY as one program path with the prompt as its single argument —
// there is no shell splitting, so a "<binary> <subcommand>" value is
// unexecutable (a live failure once shipped exactly that way: git looked for
// a file literally named "quack git-askpass").
//
// Git's two-call protocol: it invokes askpass once with a
// "Username for '<url>':" prompt and once with "Password for '<url>':". The
// prompt argument tells us which — username prompts get the configured
// username, everything else gets the token (tools.GitAskpassAnswer). Both
// values arrive via env vars quack sets ONLY on the git child process (never
// the long-lived server), so no secret ever touches disk or the server's own
// environ.
func gitAskpassMain(args []string, out io.Writer) {
	prompt := ""
	if len(args) > 1 {
		prompt = args[1]
	}
	fmt.Fprintln(out, tools.GitAskpassAnswer(prompt))
}

// isGitAskpassInvocation reports whether this process was exec'd through the
// askpass symlink (see gitAskpassMain). Split out so main() and the test
// binary's TestMain share the exact dispatch predicate.
func isGitAskpassInvocation() bool {
	return len(os.Args) > 0 && filepath.Base(os.Args[0]) == tools.GitAskpassLinkName
}

// newGitAskpassCmd keeps `quack git-askpass <prompt>` as a hidden secondary
// entry to the same logic (handy for manually debugging a credential setup);
// the mechanism git actually uses is the argv[0] dispatch above.
func newGitAskpassCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "git-askpass [prompt]",
		Hidden: true,
		Args:   cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			prompt := ""
			if len(args) > 0 {
				prompt = args[0]
			}
			fmt.Fprintln(cmd.OutOrStdout(), tools.GitAskpassAnswer(prompt))
			return nil
		},
	}
}
