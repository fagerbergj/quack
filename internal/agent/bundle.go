package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/bundledir"
	"github.com/fagerbergj/quack/internal/recordstore"
)

// Bundle is a declarative agent definition: agent-card.json + prompt.md.
// Read from disk first (live edits), then the embedded copy.
type Bundle struct {
	Card   Card
	Prompt string
	// Hash: stable digest over agent-card.json + prompt.md + rubric.yaml (if present) AS RESOLVED for this round - ledger provenance for "this bundle
	// produced this output" (#1096). rubric.yaml is read directly here rather than via vetting (would import-cycle). memory.md is deliberately excluded: its
	// content is folded into the resolved system instruction the model actually sees, already covered by that call's gen_ai.prompt.version content hash.
	Hash string
	// Dir: where the bundle came from, so ResolvePrompt can re-read its
	// sourceable parts each round. PromptSource/PromptVersion are where
	// system/<agent> came from - the llm.call ledger's prompt provenance.
	Dir           string
	PromptSource  string
	PromptVersion string
}

// Card is the agent's identity, parsed from agent-card.json. Skills are
// informational metadata about what the agent can do.
type Card struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Skills      []Skill `json:"skills,omitempty"`
	// Artifact: this job's default output kind, one of
	// recordstore.ArtifactKindNames() - stamped onto every node this agent is
	// assigned (dag.AgentInfo.DefaultArtifact) as a property of the job, not
	// a per-assignment override.
	Artifact string `json:"artifact,omitempty"`
}

// Skill is one declared capability of an agent.
type Skill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags,omitempty"`
}

const (
	cardFile   = "agent-card.json"
	promptFile = "prompt.md"
	memoryFile = "memory.md"
)

// LoadBundle reads and validates the agent bundle in dir, taking its prompt
// and rubric from res (nil resolves the shipped files). Call it again at round
// start - Resolve is the cheap path once the bundle exists.
func LoadBundle(ctx context.Context, res *artifactsrc.Resolver, dir string) (*Bundle, error) {
	rawCard, err := bundledir.ReadFile(bundledir.PathJoin(dir, cardFile))
	if err != nil {
		return nil, fmt.Errorf("agent bundle %q: read %s: %w", dir, cardFile, err)
	}
	var card Card
	if err := json.Unmarshal(rawCard, &card); err != nil {
		return nil, fmt.Errorf("agent bundle %q: parse %s: %w", dir, cardFile, err)
	}
	if strings.TrimSpace(card.Name) == "" {
		return nil, fmt.Errorf("agent bundle %q: %s has empty name", dir, cardFile)
	}
	if card.Artifact != "" {
		if err := recordstore.ValidateArtifactKind(card.Artifact); err != nil {
			return nil, fmt.Errorf("agent bundle %q: %s: artifact: %w", dir, cardFile, err)
		}
	}

	promptArt, err := resolveBundle(ctx, res, "system", dir, promptFile)
	if err != nil {
		return nil, fmt.Errorf("agent bundle %q: read %s: %w", dir, promptFile, err)
	}
	prompt := strings.TrimSpace(promptArt.Body)
	if prompt == "" {
		return nil, fmt.Errorf("agent bundle %q: %s is empty", dir, promptFile)
	}

	rubric, err := artifactsrc.ReadBundleFile(ctx, res, "rubric", dir, "rubric.yaml")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("agent bundle %q: read rubric.yaml: %w", dir, err)
	}

	h := sha256.New()
	h.Write(rawCard)
	h.Write([]byte(promptArt.Body))
	h.Write(rubric)
	hash := hex.EncodeToString(h.Sum(nil))[:16]

	return &Bundle{Card: card, Prompt: prompt, Hash: hash, Dir: dir, PromptSource: promptArt.Source, PromptVersion: promptArt.VersionID}, nil
}

// ResolvePrompt re-resolves system/<agent> for a round; a resolver failure
// keeps the bundle running on the bytes it loaded with rather than failing it.
func (b *Bundle) ResolvePrompt(ctx context.Context, res *artifactsrc.Resolver) artifactsrc.Artifact {
	boot := artifactsrc.Artifact{Body: b.Prompt, Source: b.PromptSource, VersionID: b.PromptVersion}
	name := artifactsrc.BundleName("system", b.Dir)
	if name == "" {
		return boot
	}
	art, err := res.Resolve(ctx, name)
	if err != nil {
		slog.Warn("agent prompt unresolved; keeping the loaded bundle",
			"component", "agent", "artifact", name, "err", err)
		return boot
	}
	return art
}

// resolveBundle resolves one of a bundle's sourceable files, keeping the
// artifact (its version id is the round's prompt provenance).
func resolveBundle(ctx context.Context, res *artifactsrc.Resolver, kind, dir, file string) (artifactsrc.Artifact, error) {
	if name := artifactsrc.BundleName(kind, dir); name != "" {
		return res.Resolve(ctx, name)
	}
	raw, err := bundledir.ReadFile(bundledir.PathJoin(dir, file))
	if err != nil {
		return artifactsrc.Artifact{}, err
	}
	return artifactsrc.Artifact{Body: string(raw), Source: artifactsrc.StaticSource}, nil
}

// LoadBundleMemory resolves the bundle's optional memory.md ("" when it has none).
func LoadBundleMemory(ctx context.Context, res *artifactsrc.Resolver, dir string) (string, error) {
	raw, err := artifactsrc.ReadBundleFile(ctx, res, "memory", dir, memoryFile)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("agent bundle %q: read %s: %w", dir, memoryFile, err)
	}
	return strings.TrimSpace(string(raw)), nil
}
