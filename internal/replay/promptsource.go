package replay

import (
	"context"
	"fmt"
	"strconv"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/langfuse"
)

// recordedVersion is one artifact name's provenance recorded on an llm.call.
type recordedVersion struct {
	source  string
	version string
}

// ref formats name@version for an error message.
func (rv recordedVersion) ref(name string) string { return name + "@" + rv.version }

// recordedPrompts collects prompt provenance per artifact name (from
// quack.prompt.artifact, #1422); a name recorded at more than one version -
// rounds re-resolve by design - refuses, naming both, rather than guessing.
func (s *Session) recordedPrompts() (map[string]recordedVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]recordedVersion{}
	for _, st := range s.streams {
		for _, ce := range st.chat {
			if ce.PromptSource == "" || ce.PromptVersionID == "" || ce.PromptArtifact == "" {
				continue
			}
			name := ce.PromptArtifact
			rv := recordedVersion{source: ce.PromptSource, version: ce.PromptVersionID}
			prev, ok := out[name]
			if !ok {
				out[name] = rv
				continue
			}
			if prev != rv {
				return nil, fmt.Errorf("replay: %s moved versions mid-run (%s vs %s); refusing to guess which round to pin",
					name, prev.ref(name), rv.ref(name))
			}
		}
	}
	return out, nil
}

// PromptSource is an artifactsrc.Source pinning every name to the exact
// version a recorded run used (#1422): NewPromptSource resolves eagerly, so
// an unreproducible version fails replay's setup, not Resolver.fetch's fallback.
type PromptSource struct {
	resolved map[string]artifactsrc.Artifact
}

// NewPromptSource resolves every artifact name sess's recorded calls used.
// lf is nil when no prompts.store is configured; storeName is that store's
// name, to refuse a version recorded from a different one (#1422).
func NewPromptSource(ctx context.Context, sess *Session, lf *langfuse.Client, storeName string) (*PromptSource, error) {
	recorded, err := sess.recordedPrompts()
	if err != nil {
		return nil, err
	}
	resolved := map[string]artifactsrc.Artifact{}
	for name, rec := range recorded {
		art, err := resolveRecorded(ctx, name, rec, lf, storeName)
		if err != nil {
			return nil, err
		}
		resolved[name] = art
	}
	return &PromptSource{resolved: resolved}, nil
}

// resolveRecorded resolves and verifies one name@version: static must still
// hash to the recorded id; langfuse is fetched by that exact version, only
// from the store it was recorded from.
func resolveRecorded(ctx context.Context, name string, rec recordedVersion, lf *langfuse.Client, storeName string) (artifactsrc.Artifact, error) {
	ref := rec.ref(name)
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
		return artifactsrc.Artifact{}, fmt.Errorf("replay: %s: recorded from store %q, but this deployment has no prompts.store configured", ref, rec.source)
	}
	if rec.source != storeName {
		return artifactsrc.Artifact{}, fmt.Errorf("replay: %s: recorded from store %q, but this deployment's prompts.store is %q", ref, rec.source, storeName)
	}
	version, err := strconv.Atoi(rec.version)
	if err != nil {
		return artifactsrc.Artifact{}, fmt.Errorf("replay: %s: recorded version is not numeric: %w", ref, err)
	}
	p, ok, err := lf.GetPrompt(ctx, name, langfuse.GetPromptOpts{Version: version})
	if err != nil {
		if langfuse.IsTransient(err) {
			return artifactsrc.Artifact{}, fmt.Errorf("replay: %s: langfuse unreachable, replay not attempted: %w", ref, err)
		}
		return artifactsrc.Artifact{}, fmt.Errorf("replay: %s: %w", ref, err)
	}
	if !ok {
		return artifactsrc.Artifact{}, fmt.Errorf("replay: %s: version no longer exists in langfuse", ref)
	}
	return artifactsrc.Artifact{Name: name, Body: p.Body, Config: p.Config, Source: rec.source, VersionID: strconv.Itoa(p.Version)}, nil
}

// Get returns the pinned artifact for name, or (_, false, nil) for a
// pre-#1422 entry, or a known gap: rubric/*, memory/*, compaction and
// acp.environment carry no per-call provenance, so they resolve live, unpinned.
func (s *PromptSource) Get(_ context.Context, name string) (artifactsrc.Artifact, bool, error) {
	art, ok := s.resolved[name]
	return art, ok, nil
}

// Seed is a no-op: replay never seeds a store.
func (s *PromptSource) Seed(context.Context, string, artifactsrc.Artifact) error { return nil }
