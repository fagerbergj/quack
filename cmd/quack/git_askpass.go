package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/fagerbergj/quack/internal/tools"
)

// newGitAskpassCmd is the hidden GIT_ASKPASS target the git tools
// (internal/tools/git.go's runGit) point git at for a credentialed operation:
// `git_ask := "<this binary> git-askpass"`. Git invokes it with a text prompt
// ("Username for ...", "Password for ...") as its one argument and reads the
// credential from stdout — this command ignores the prompt text (the git
// tools never rely on git to prompt for a username; see gitEnv/credentialFor,
// which supply the credential's username/token directly) and simply prints
// the value of tools.GitAskpassTokenEnv, an env var quack sets ONLY on this
// one git child process (never on the long-lived server process itself, so it
// never appears in that process's `ps`/environ). Hidden: this is a mechanism,
// not a user-facing command.
func newGitAskpassCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "git-askpass [prompt]",
		Hidden: true,
		Args:   cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(), os.Getenv(tools.GitAskpassTokenEnv))
			return nil
		},
	}
}
