package langfuse

import (
	"context"
	"fmt"
	"strconv"
	"strings"

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
	return artifactsrc.Artifact{Name: name, Body: p.Body, Config: p.Config, VersionID: strconv.Itoa(p.Version)}, true, nil // Source blank: the resolver stamps the store name
}

// Seed is a no-op: an experiment run never seeds a store.
func (s *PinnedSource) Seed(context.Context, string, artifactsrc.Artifact) error { return nil }

// ParsePin splits a --prompt value "system/<agent>@N" into its artifact name and version.
func ParsePin(s string) (name string, version int, err error) {
	name, v, ok := strings.Cut(s, "@")
	if !ok || name == "" {
		return "", 0, fmt.Errorf("--prompt %q: want <name>@<version>", s)
	}
	version, err = strconv.Atoi(v)
	if err != nil || version <= 0 {
		return "", 0, fmt.Errorf("--prompt %q: version must be a positive integer", s)
	}
	return name, version, nil
}
