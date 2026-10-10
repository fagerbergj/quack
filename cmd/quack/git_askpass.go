package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/fagerbergj/quack/internal/tools"
)

// gitAskpassMain is quack's GIT_ASKPASS mode, reached by argv[0] dispatch via the <workspace root>/.quack-askpass
// symlink: git execs $GIT_ASKPASS as one program path, so a "<binary> <subcommand>" value can't work.
func gitAskpassMain(args []string, out io.Writer) {
	prompt := ""
	if len(args) > 1 {
		prompt = args[1]
	}
	fmt.Fprintln(out, tools.GitAskpassAnswer(prompt))
}

// isGitAskpassInvocation reports whether this process was exec'd through the askpass symlink;
// shared by main() and the test binary's TestMain.
func isGitAskpassInvocation() bool {
	return len(os.Args) > 0 && filepath.Base(os.Args[0]) == tools.GitAskpassLinkName
}

// newGitAskpassCmd is a hidden `quack git-askpass <prompt>` entry to the same logic, for debugging
// credentials by hand; git itself uses the argv[0] dispatch.
func newGitAskpassCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "git-askpass [prompt]",
		Short:  "Answer a git credential prompt (debugging aid; git normally invokes this via argv[0])",
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
