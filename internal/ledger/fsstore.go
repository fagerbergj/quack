package ledger

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// FSStore is the v1 LedgerStore adapter: one JSONL file per session under
// root, one line per Append. Every Append opens, writes, syncs, and closes -
// no cached file handle - so a kill -9 mid-run leaves every completed write
// durable and the file never holds a long-lived fd across a whole chat.
//
// mu serializes ALL sessions through one critical section (the "single
// writer" the design calls for): simplest correct answer for a local-disk
// JSONL append, and cheap enough that cross-session contention is not a
// concern at quack's scale.
type FSStore struct {
	root string
	mu   sync.Mutex
}

// NewFSStore returns an FSStore rooted at root, creating it if needed.
func NewFSStore(root string) (*FSStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("ledger: filesystem store needs a root")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("ledger: mkdir root %q: %w", root, err)
	}
	return &FSStore{root: root}, nil
}

// path resolves sessionID to its JSONL file, rejecting anything that could
// escape root (a session id is a chat id, never a path).
func (s *FSStore) path(sessionID string) (string, error) {
	if sessionID == "" || strings.ContainsAny(sessionID, "/\\") || sessionID == "." || sessionID == ".." {
		return "", fmt.Errorf("ledger: invalid session id %q", sessionID)
	}
	return filepath.Join(s.root, sessionID+".jsonl"), nil
}

func (s *FSStore) Append(_ context.Context, sessionID string, entry []byte) error {
	p, err := s.path(sessionID)
	if err != nil {
		return err
	}
	line := make([]byte, 0, len(entry)+1)
	line = append(line, entry...)
	line = append(line, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("ledger: open %q: %w", p, err)
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("ledger: write %q: %w", p, err)
	}
	// fsync so a crash immediately after this call leaves the line durable -
	// the whole point of the crash-safety guarantee.
	return f.Sync()
}

func (s *FSStore) ReadStream(_ context.Context, sessionID string) (io.ReadCloser, error) {
	p, err := s.path(sessionID)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, fmt.Errorf("ledger: open %q: %w", p, err)
	}
	return f, nil
}

func (s *FSStore) List(_ context.Context) ([]SessionRef, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, fmt.Errorf("ledger: read root %q: %w", s.root, err)
	}
	out := make([]SessionRef, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue // best-effort listing; skip an entry that vanished mid-scan
		}
		out = append(out, SessionRef{
			ID:      strings.TrimSuffix(e.Name(), ".jsonl"),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
	}
	return out, nil
}

func (s *FSStore) Delete(_ context.Context, sessionID string) error {
	p, err := s.path(sessionID)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("ledger: delete %q: %w", p, err)
	}
	return nil
}
