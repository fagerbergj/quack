package replay

import (
	"context"
	"fmt"
	"strconv"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/langfuse"
)

// recordedVersion is one artifact name's provenance, taken from the first
// recorded llm.call entry that used it - one recording carries one version per
// name in practice (nothing re-resolves mid-run live either, epic #1418).
type recordedVersion struct {
	source  string
	version string
}

// recordedPrompts collects prompt_source/prompt_version_id per artifact name
// ("system/<agent>") from every stream's chat entries; an entry missing either
// field (pre-P1) is left out, so its name falls through to normal resolution.
func (s *Session) recordedPrompts() map[string]recordedVersion {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]recordedVersion{}
	for _, st := range s.streams {
		for _, ce := range st.chat {
			if ce.PromptSource == "" || ce.PromptVersionID == "" || ce.PromptName == "" {
				continue
			}
			name := "system/" + ce.PromptName
			if _, ok := out[name]; !ok {
				out[name] = recordedVersion{source: ce.PromptSource, version: ce.PromptVersionID}
			}
		}
	}
	return out
}

// PromptSource is an artifactsrc.Source pinning every name to the exact
// version a recorded run used (#1422): NewPromptSource resolves eagerly, so
// an unreproducible version fails replay's setup, not Resolver.fetch's fallback.
type PromptSource struct {
	resolved map[string]artifactsrc.Artifact
}

// NewPromptSource resolves every artifact name sess's recorded calls used.
// lf is nil when no prompts.store is configured; a name recorded from
// langfuse then refuses naming that requirement.
func NewPromptSource(ctx context.Context, sess *Session, lf *langfuse.Client) (*PromptSource, error) {
	resolved := map[string]artifactsrc.Artifact{}
	for name, rec := range sess.recordedPrompts() {
		art, err := resolveRecorded(ctx, name, rec, lf)
		if err != nil {
			return nil, err
		}
		resolved[name] = art
	}
	return &PromptSource{resolved: resolved}, nil
}

// resolveRecorded resolves and verifies one name@version: static must still
// hash to the recorded id; langfuse is fetched by that exact version.
func resolveRecorded(ctx context.Context, name string, rec recordedVersion, lf *langfuse.Client) (artifactsrc.Artifact, error) {
	ref := name + "@" + rec.version
	if rec.source == artifactsrc.StaticSource {
		art, err := artifactsrc.Static(name)
		if err != nil {
			return artifactsrc.Artifact{}, fmt.Errorf("replay: %s: %w", ref, err)
		}
		if art.VersionID != rec.version {
			return artifactsrc.Artifact{}, fmt.Errorf("replay: %s: shipped file now hashes to %s, refusing to replay a different version", ref, art.VersionID)
		}
		return art, nil
	}
	if lf == nil {
		return artifactsrc.Artifact{}, fmt.Errorf("replay: %s: recorded from langfuse store %q, but this deployment has no prompts.store configured", ref, rec.source)
	}
	version, err := strconv.Atoi(rec.version)
	if err != nil {
		return artifactsrc.Artifact{}, fmt.Errorf("replay: %s: recorded version is not numeric: %w", ref, err)
	}
	p, ok, err := lf.GetPrompt(ctx, name, langfuse.GetPromptOpts{Version: version})
	if err != nil {
		return artifactsrc.Artifact{}, fmt.Errorf("replay: %s: %w", ref, err)
	}
	if !ok {
		return artifactsrc.Artifact{}, fmt.Errorf("replay: %s: version no longer exists in langfuse", ref)
	}
	return artifactsrc.Artifact{Name: name, Body: p.Body, Config: p.Config, Source: rec.source, VersionID: strconv.Itoa(p.Version)}, nil
}

// Get returns the pinned artifact for name, or (_, false, nil) when sess
// recorded no provenance for it (a pre-P1 entry, or an unpinned name like a
// rubric/memory file) - the resolver then falls back to normal resolution.
func (s *PromptSource) Get(_ context.Context, name string) (artifactsrc.Artifact, bool, error) {
	art, ok := s.resolved[name]
	return art, ok, nil
}

// Seed is a no-op: replay never seeds a store.
func (s *PromptSource) Seed(context.Context, string, artifactsrc.Artifact) error { return nil }
