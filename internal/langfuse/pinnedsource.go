package langfuse

import (
	"context"
	"strconv"

	"github.com/fagerbergj/quack/internal/artifactsrc"
)

// PinnedSource pins names to exact Langfuse versions (--prompt system/<agent>@N),
// mirroring replay's PromptSource (#1422 P3) but built from an explicit Pins map.
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
