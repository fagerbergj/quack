package agent

// Registers the Sleeper agents' artifact kinds (lineup/waivers/trends/
// season-notes) for this package's tests: TestGoldenAgentPrompts walks every
// shipped bundle, including the three that set card.Artifact, and production
// internal/agent has no reason to import a sleeper-specific package itself.
import _ "github.com/fagerbergj/quack/internal/sleeperkinds"
