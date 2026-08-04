package store

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// instanceIDFile names the marker LoadOrCreateInstanceID reads/writes. Not
// "the database" on purpose: the instance identity has to be readable before
// this process can prove it's the same one that wrote any given row, so it
// lives on local disk alongside the deployment it belongs to (dir is
// typically QUACK_WORKSPACE_ROOT, a persistent volume across a container
// restart, but a fresh volume - i.e. a genuinely new deployment - gets a
// fresh id).
const instanceIDFile = ".instance-id"

// LoadOrCreateInstanceID returns dir's persisted server identity, creating
// one on first use. Meant only for a process that intends to own the DAG run
// loop (`quack server run`) - pass the result to Store.SetInstanceID before
// calling FailStaleDagNodes so a restart reconciles what it left mid-run
// last time, without matching a peer's still-live rows (#683).
func LoadOrCreateInstanceID(dir string) (string, error) {
	path := filepath.Join(dir, instanceIDFile)
	if b, err := os.ReadFile(path); err == nil {
		if id := strings.TrimSpace(string(b)); id != "" {
			return id, nil
		}
	}
	id := uuid.NewString()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(id), 0o644); err != nil {
		return "", err
	}
	return id, nil
}
