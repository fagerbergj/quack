package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/fagerbergj/quack/internal/bundledir"
)

// Bundle is a declarative agent definition: agent-card.json + prompt.md.
// Read from disk first (live edits), then the embedded copy.
type Bundle struct {
	Card   Card
	Prompt string
	// Hash: stable digest over agent-card.json + prompt.md + rubric.yaml (if
	// present), computed once at load - ledger provenance for "this bundle
	// produced this output" (#1096). rubric.yaml is optional and read
	// directly here rather than via vetting (would import-cycle).
	Hash string
}

// Card is the agent's identity, parsed from agent-card.json. Skills are
// informational metadata about what the agent can do.
type Card struct {
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Skills      []Skill `json:"skills,omitempty"`
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

// LoadBundle reads and validates the agent bundle in dir.
func LoadBundle(dir string) (*Bundle, error) {
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

	rawPrompt, err := bundledir.ReadFile(bundledir.PathJoin(dir, promptFile))
	if err != nil {
		return nil, fmt.Errorf("agent bundle %q: read %s: %w", dir, promptFile, err)
	}
	prompt := strings.TrimSpace(string(rawPrompt))
	if prompt == "" {
		return nil, fmt.Errorf("agent bundle %q: %s is empty", dir, promptFile)
	}

	rubric, err := bundledir.ReadFile(bundledir.PathJoin(dir, "rubric.yaml"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("agent bundle %q: read rubric.yaml: %w", dir, err)
	}

	h := sha256.New()
	h.Write(rawCard)
	h.Write(rawPrompt)
	h.Write(rubric)
	hash := hex.EncodeToString(h.Sum(nil))[:16]

	return &Bundle{Card: card, Prompt: prompt, Hash: hash}, nil
}

// LoadBundleMemory reads an optional memory.md from the bundle directory.
func LoadBundleMemory(dir string) (string, error) {
	raw, err := bundledir.ReadFile(bundledir.PathJoin(dir, memoryFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("agent bundle %q: read %s: %w", dir, memoryFile, err)
	}
	return strings.TrimSpace(string(raw)), nil
}
