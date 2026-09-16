package langfuse

import (
	"context"
	"strconv"

	"github.com/fagerbergj/quack/internal/artifactsrc"
)

// PinnedSource is an artifactsrc.Source that pins specific names to specific Langfuse
// versions (`quack experiment run --prompt system/<agent>@N`), following the same
// per-name-pin pattern as replay's PromptSource (#1422 P3) but built from an explicit
// Pins map instead of a recorded session.
type PinnedSource struct {
	Client *Client
	Pins   map[string]int // artifact name -> version
}

// Get returns name's pinned version, or (_, false, nil) when name isn't pinned - the
// Resolver then falls back to its normal Source/static chain, unaffected.
func (s *PinnedSource) Get(ctx context.Context, name string) (artifactsrc.Artifact, bool, error) {
	version, pinned := s.Pins[name]
	if !pinned {
		return artifactsrc.Artifact{}, false, nil
	}
	p, ok, err := s.Client.GetPrompt(ctx, name, GetPromptOpts{Version: version})
	if err != nil || !ok {
		return artifactsrc.Artifact{}, false, err
	}
	return artifactsrc.Artifact{Name: name, Body: p.Body, Config: p.Config, Source: "langfuse", VersionID: strconv.Itoa(p.Version)}, true, nil
}

// Seed is a no-op: an experiment run never seeds a store.
func (s *PinnedSource) Seed(context.Context, string, artifactsrc.Artifact) error { return nil }
