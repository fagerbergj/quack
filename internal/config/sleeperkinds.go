package config

// config.Load validates a bound node's artifact: kind at load time, so the
// registry must be linked here, not left to whatever else a binary imports.
import _ "github.com/fagerbergj/quack/internal/sleeperkinds"
