package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Bundle is a declarative agent definition loaded from disk: an agent-card.json
// (identity + skills) plus a prompt.md (the system instruction). Config binds the
// model and the built-in tool selection separately, so defining a new agent is
// just dropping a bundle directory and adding a config entry — no recompile.
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
)

// LoadBundle reads and validates the agent bundle in dir.
func LoadBundle(dir string) (*Bundle, error) {
	rawCard, err := os.ReadFile(filepath.Join(dir, cardFile))
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

	rawPrompt, err := os.ReadFile(filepath.Join(dir, promptFile))
	if err != nil {
		return nil, fmt.Errorf("agent bundle %q: read %s: %w", dir, promptFile, err)
	}
	prompt := strings.TrimSpace(string(rawPrompt))
	if prompt == "" {
		return nil, fmt.Errorf("agent bundle %q: %s is empty", dir, promptFile)
	}

	return &Bundle{Card: card, Prompt: prompt}, nil
}
