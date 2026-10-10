package agent

import (
	"context"
	"strings"

	"github.com/fagerbergj/quack/internal/artifactsrc"
)

// compactionPrompts resolves system/compaction and system/compaction.summary, a drop-and-summarize strategy
// (as in GOOSE/OpenHands) rather than blanking old results in place.
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
