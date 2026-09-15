package langfuse

import (
	"context"
	"fmt"
	"strings"
)

// SeedTag marks a seeded version for filtering in the Langfuse UI only;
// detecting a seed vs. an operator edit relies on the commit message prefix, not this tag.
const SeedTag = "quack-seed"

const seedCommitPrefix = SeedTag + " "

// SeedAction reports what Seed did.
type SeedAction string

const (
	Created        SeedAction = "created"
	Updated        SeedAction = "updated"
	Unchanged      SeedAction = "unchanged"
	OperatorEdited SeedAction = "operator-edited"
)

// Seed applies #1418's seeding rules: create on 404 (no production label; Langfuse adds
// "latest" itself), re-seed only when the latest version is itself a stale seed, and
// never touch a version a person authored.
func (c *Client) Seed(ctx context.Context, name, staticBody, staticHash string) (SeedAction, error) {
	p, found, err := c.GetPrompt(ctx, name, GetPromptOpts{Label: "latest"})
	if err != nil {
		return "", fmt.Errorf("langfuse: seed %q: %w", name, err)
	}
	if !found {
		if _, err := c.CreatePrompt(ctx, CreatePromptRequest{
			Name:          name,
			Body:          staticBody,
			Config:        map[string]any{},
			Tags:          []string{SeedTag},
			CommitMessage: seedCommitPrefix + staticHash,
		}); err != nil {
			return "", fmt.Errorf("langfuse: seed %q: create: %w", name, err)
		}
		return Created, nil
	}

	if !strings.HasPrefix(p.CommitMessage, seedCommitPrefix) {
		return OperatorEdited, nil
	}

	prevHash := strings.TrimSpace(strings.TrimPrefix(p.CommitMessage, seedCommitPrefix))
	if prevHash == staticHash {
		return Unchanged, nil
	}

	if _, err := c.CreatePrompt(ctx, CreatePromptRequest{
		Name:          name,
		Body:          staticBody,
		Config:        map[string]any{},
		Tags:          unionTag(p.Tags, SeedTag),
		CommitMessage: seedCommitPrefix + staticHash,
	}); err != nil {
		return "", fmt.Errorf("langfuse: seed %q: create: %w", name, err)
	}
	return Updated, nil
}

// unionTag adds tag to tags if missing, preserving order and dropping duplicates.
// Every POST replaces a prompt's whole tag set, so an update must carry forward
// whatever operator tags already exist rather than overwriting them with just SeedTag.
func unionTag(tags []string, tag string) []string {
	out := make([]string, 0, len(tags)+1)
	seen := false
	for _, t := range tags {
		if t == tag {
			seen = true
		}
		out = append(out, t)
	}
	if !seen {
		out = append(out, tag)
	}
	return out
}

// Resolve fetches the client's pinned label (WithPinLabel), falling back to latest
// only when the pin label itself isn't found. Returns (_, false, nil) if neither is found.
func (c *Client) Resolve(ctx context.Context, name string) (Prompt, bool, error) {
	if c.pinLabel != "" {
		p, found, err := c.GetPrompt(ctx, name, GetPromptOpts{Label: c.pinLabel})
		if err != nil {
			return Prompt{}, false, fmt.Errorf("langfuse: resolve %q label %q: %w", name, c.pinLabel, err)
		}
		if found {
			return p, true, nil
		}
	}
	p, found, err := c.GetPrompt(ctx, name, GetPromptOpts{Label: "latest"})
	if err != nil {
		return Prompt{}, false, fmt.Errorf("langfuse: resolve %q label latest: %w", name, err)
	}
	return p, found, nil
}
