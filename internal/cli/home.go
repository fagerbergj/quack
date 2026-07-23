package cli

import (
	"os"
	"path/filepath"
)

// Home is the quack CLI's home directory: the one place the CLI keeps its own
// state (the server registry, the daemon pidfile/log, the managed-stores
// compose). Honors $QUACK_HOME, else ~/.quack - the ~/.kube / ~/.docker model
// (one self-contained dir, easy to find, back up, or rm -rf). Project config
// (quack.yaml) is separate - it stays in the project directory.
func Home() string {
	if d := os.Getenv("QUACK_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".quack")
}
