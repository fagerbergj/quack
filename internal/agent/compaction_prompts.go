package agent

import (
	"context"
	"strings"

	"github.com/fagerbergj/quack/internal/artifactsrc"
)

// compactionPrompts resolves the summarizer's system prompt and output
// template (system/compaction, system/compaction.summary). They follow
// GOOSE/OpenHands's drop-and-summarize strategy, not blank-the-old-result-in-place.
func compactionPrompts(ctx context.Context, res *artifactsrc.Resolver) (system, template string, err error) {
	sys, err := res.Resolve(ctx, "system/compaction")
	if err != nil {
		return "", "", err
	}
	tmpl, err := res.Resolve(ctx, "system/compaction.summary")
	if err != nil {
		return "", "", err
	}
	return strings.TrimSpace(sys.Body), strings.TrimSpace(tmpl.Body), nil
}
