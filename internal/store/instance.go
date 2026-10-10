package store

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// instanceIDFile lives on local disk, not in the database: identity must be readable before this process can
// prove it wrote any row. A fresh volume (a new deployment) gets a fresh id.
const instanceIDFile = ".instance-id"

// LoadOrCreateInstanceID returns dir's persisted server identity, creating it on first use. Only for the
// process owning the DAG run loop, so a restart reclaims its own rows but never a live peer's.
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
