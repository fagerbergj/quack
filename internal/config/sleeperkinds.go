package config

// Loading validates a bound node's artifact: kind, so the registry must be
// linked here, not left to whatever else a binary imports.
import _ "github.com/fagerbergj/quack/internal/sleeperkinds"
