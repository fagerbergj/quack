package langfuse

import (
	"context"
	"fmt"
	"strings"
)

// SeedTag marks a version as machine-created by Seed, distinguishing it from operator edits.
const SeedTag = "quack-seed"

const seedCommitPrefix = SeedTag + " "

// Seed rules per the epic (#1418 Decisions): GET latest; 404 -> create with empty
// config, tag quack-seed, no label; latest is itself a seed with a different hash ->
// create a new seeded version; latest was operator-authored -> never touch it.
// Actions: "created", "updated", "unchanged", "operator-edited".
func Seed(ctx context.Context, client *Client, name, staticBody, staticHash string) (string, error) {
	p, found, err := client.GetPrompt(ctx, name, GetPromptOpts{Label: "latest"})
	if err != nil {
		return "", err
	}
	if !found {
		_, err := client.CreatePrompt(ctx, CreatePromptRequest{
			Name:          name,
			Body:          staticBody,
			Config:        map[string]any{},
			Tags:          []string{SeedTag},
			CommitMessage: seedCommitPrefix + staticHash,
		})
		if err != nil {
			return "", err
		}
		return "created", nil
	}

	if !strings.HasPrefix(p.CommitMessage, seedCommitPrefix) {
		return "operator-edited", nil
	}

	prevHash := strings.TrimPrefix(p.CommitMessage, seedCommitPrefix)
	if prevHash == staticHash {
		return "unchanged", nil
	}

	_, err = client.CreatePrompt(ctx, CreatePromptRequest{
		Name:          name,
		Body:          staticBody,
		Config:        map[string]any{},
		Tags:          []string{SeedTag},
		CommitMessage: seedCommitPrefix + staticHash,
	})
	if err != nil {
		return "", err
	}
	return "updated", nil
}

// Resolve fetches the pinned version (label=pinLabel), falling back to latest
// when the pin label doesn't exist. Returns (_, false, nil) if neither is found.
func Resolve(ctx context.Context, client *Client, name, pinLabel string) (Prompt, bool, error) {
	if pinLabel != "" {
		p, found, err := client.GetPrompt(ctx, name, GetPromptOpts{Label: pinLabel})
		if err != nil {
			return Prompt{}, false, fmt.Errorf("langfuse: resolve %q label %q: %w", name, pinLabel, err)
		}
		if found {
			return p, true, nil
		}
	}
	p, found, err := client.GetPrompt(ctx, name, GetPromptOpts{Label: "latest"})
	if err != nil {
		return Prompt{}, false, fmt.Errorf("langfuse: resolve %q label latest: %w", name, err)
	}
	return p, found, nil
}
