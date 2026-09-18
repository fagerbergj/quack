package config

// Registers the Sleeper artifact kinds (lineup/waivers/trends/season-notes)
// for this package's tests: config/quack.yaml's shipped workflows: shapes
// name them as bound-node artifacts, and validateWorkflowNodes checks each
// against recordstore's registry at Load time - production internal/config
// has no reason to import a sleeper-specific package itself.
import _ "github.com/fagerbergj/quack/internal/sleeperkinds"
