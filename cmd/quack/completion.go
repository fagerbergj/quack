package main

import (
	"context"
	"sort"

	"github.com/spf13/cobra"

	"github.com/fagerbergj/quack/internal/cli"
	"github.com/fagerbergj/quack/internal/config"
)

// completeWithTarget resolves --server, then hands the target to fn; any
// failure degrades to no completions rather than a shell-visible error.
func completeWithTarget(cmd *cobra.Command, fn func(ctx context.Context, target string) ([]string, error)) ([]string, cobra.ShellCompDirective) {
	server, _ := cmd.Flags().GetString("server")
	target, stop, err := resolveTarget(cmd.Context(), server)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	defer stop()
	ids, err := fn(cmd.Context(), target)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return ids, cobra.ShellCompDirectiveNoFileComp
}

// completeChatIDs completes a bare <chat-id> positional.
func completeChatIDs(cmd *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) != 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return completeWithTarget(cmd, cli.CompletionChatIDs)
}

// completeChatThenNodeIDs backs the `chat node <verb>` tree: <chat-id> first,
// then <node-id> scoped to that chat's latest run.
func completeChatThenNodeIDs(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	switch len(args) {
	case 0:
		return completeChatIDs(cmd, args, toComplete)
	case 1:
		chatID := args[0]
		return completeWithTarget(cmd, func(ctx context.Context, target string) ([]string, error) {
			return cli.CompletionNodeIDs(ctx, target, chatID)
		})
	default:
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
}

// completeMemoryIDs completes a bare <memory-id> positional.
func completeMemoryIDs(cmd *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) != 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return completeWithTarget(cmd, cli.CompletionMemoryIDs)
}

// completeServerNames completes a registered server's <name> - offline, from
// the local client registry (`server use|remove|login`).
func completeServerNames(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
	if len(args) != 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	c, err := cli.LoadClient()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := make([]string, 0, len(c.Servers))
	for name := range c.Servers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, cobra.ShellCompDirectiveNoFileComp
}

// completeAgentNames completes `--agent` from the local quack.yaml - offline,
// no server round trip.
func completeAgentNames(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	cfg, err := config.LoadForSandbox(defaultConfigPath())
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return cli.SandboxAgentNames(cfg), cobra.ShellCompDirectiveNoFileComp
}
