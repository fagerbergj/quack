package remarkable

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

const stateFileName = "state.json"

// docState is the per-document dispatch record: what we last sent and what
// happened to the run it started.
type docState struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Folder       string    `json:"folder,omitempty"`
	LastModified time.Time `json:"last_modified"`

	// InFlight is true from a successful Dispatch call until RunEnded
	// fires - it suppresses a re-dispatch of a run still in progress.
	InFlight bool `json:"in_flight"`

	// LastOutcome/LastError describe the most recent finished run.
	LastOutcome string `json:"last_outcome,omitempty"`
	LastError   string `json:"last_error,omitempty"`

	// Attempts counts how many times this document has been dispatched.
	Attempts  int       `json:"attempts"`
	UpdatedAt time.Time `json:"updated_at"`
}

// state is persisted as one JSON file in Host.DataDir; writes serialize through extension.mu.
type state struct {
	Documents map[string]docState `json:"documents"`
}

func loadState(path string) (*state, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &state{Documents: map[string]docState{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, err
	}
	if st.Documents == nil {
		st.Documents = map[string]docState{}
	}
	return &st, nil
}

// save writes atomically (tmp file + rename) so a crash mid-write can never
// leave a half-written state.json behind.
func (s *state) save(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func statePath(dataDir string) string {
	return filepath.Join(dataDir, stateFileName)
}
