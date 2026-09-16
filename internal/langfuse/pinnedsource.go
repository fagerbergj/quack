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
	if err != nil {
		return artifactsrc.Artifact{}, false, err
	}
	if !ok {
		// A pinned name's version 404s: fail loudly rather than let ChainSource
		// fall through to the store's unpinned version, which would silently
		// break the "llm.call rows carry that exact version" guarantee.
		return artifactsrc.Artifact{}, false, fmt.Errorf("pinned prompt %s@%d not found", name, version)
	}
	return artifactsrc.Artifact{Name: name, Body: p.Body, Config: p.Config, VersionID: strconv.Itoa(p.Version)}, true, nil // Source blank: the resolver stamps the store name
}

// Seed is a no-op: an experiment run never seeds a store.
func (s *PinnedSource) Seed(context.Context, string, artifactsrc.Artifact) error { return nil }

// ResolveNow resolves name's pin eagerly, before the server boots - a bad --prompt
// (unknown name/version, a 404) must fail the command naming it, not silently fall
// back to the shipped static prompt the first time the resolver hits it.
func (s *PinnedSource) ResolveNow(ctx context.Context, name string) error {
	_, ok, err := s.Get(ctx, name)
	if err != nil {
		return fmt.Errorf("--prompt %s@%d: %w", name, s.Pins[name], err)
	}
	if !ok {
		return fmt.Errorf("--prompt %s@%d: not found", name, s.Pins[name])
	}
	return nil
}

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
