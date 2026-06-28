package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fagerbergj/quack/internal/bundledir"
)

// Bundle is a declarative agent definition: an agent-card.json (identity +
// skills) plus a prompt.md (the system instruction). Config binds the model
// and the built-in tool selection separately, so defining a new agent is just
// dropping a bundle directory (under agents/) and adding a config entry — no
// recompile. Bundles are read from disk in cwd first (live repo edits), then
// the embedded copy (so an installed binary works from any directory).
type Bundle struct {
	Card   Card
	Prompt string
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

// LoadBundle reads and validates the agent bundle in dir (e.g. "agents/orchestrator").
// dir is resolved from disk in cwd first, then the embedded copy.
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

	return &Bundle{Card: card, Prompt: prompt}, nil
}

// LoadBundleMemory reads an optional memory.md from the bundle directory — the
// agent's "what to remember" guidance (M6). Returns "" if absent or empty. It is
// appended to the agent's behaviour only when the memory feature is on, so the
// guidance never dangles (and references no tools) when memory is disabled. This
// is the second optional bundle file alongside rubric.md.
func LoadBundleMemory(dir string) (string, error) {
	raw, err := bundledir.ReadFile(bundledir.PathJoin(dir, memoryFile))
	if err != nil {
		// Absent on both disk and embedded ⇒ no memory guidance (not an error).
		if isNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("agent bundle %q: read %s: %w", dir, memoryFile, err)
	}
	return strings.TrimSpace(string(raw)), nil
}

// isNotExist reports whether err is a not-exist error from either os or embed.
func isNotExist(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "no such file") || strings.Contains(err.Error(), "does not exist"))
}
