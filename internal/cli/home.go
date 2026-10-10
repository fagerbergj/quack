package cli

import (
	"os"
	"path/filepath"
)

// Home is the CLI's own state dir (server registry, daemon pidfile/log, managed-stores compose):
// $QUACK_HOME, else ~/.quack. Project config (quack.yaml) stays in the project directory.
func Home() string {
	if d := os.Getenv("QUACK_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".quack")
}
