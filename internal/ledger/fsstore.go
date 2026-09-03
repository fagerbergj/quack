package ledger

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
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

	// seqMu/seqs back AppendIntent's seq counter: in-memory only, so it is
	// NOT gapless across a restart or a second process sharing root.
	// ponytail: good enough for a filesystem store nobody runs concurrent
	// multi-process today; move to PGStore (real gapless seq) if that changes.
	seqMu sync.Mutex
	seqs  map[string]int64
}

// NewFSStore returns an FSStore rooted at root, creating it if needed.
func NewFSStore(root string) (*FSStore, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("ledger: filesystem store needs a root")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("ledger: mkdir root %q: %w", root, err)
	}
	return &FSStore{root: root, seqs: make(map[string]int64)}, nil
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

// AppendIntent is a plain best-effort append, NOT the transactional,
// gapless allocation the interface promises for Postgres: seq comes from an
// in-memory counter (see FSStore.seqs), so it is not durable and not
// coordinated across processes. Documented per the LedgerStore contract.
func (s *FSStore) AppendIntent(ctx context.Context, entry Entry) (int64, error) {
	if entry.ChatID == "" || entry.Kind == "" {
		return 0, fmt.Errorf("ledger: intent needs chat_id and kind")
	}
	seq, err := s.nextIntentSeq(ctx, entry.ChatID)
	if err != nil {
		return 0, err
	}
	entry.Seq = seq

	if entry.At.IsZero() {
		entry.At = time.Now().UTC()
	}
	body, err := json.Marshal(entry)
	if err != nil {
		return 0, fmt.Errorf("ledger: encode intent: %w", err)
	}
	if err := s.Append(ctx, entry.ChatID, body); err != nil {
		return 0, err
	}
	return entry.Seq, nil
}

// nextIntentSeq lazily seeds a chat's in-memory counter from the file's own
// max seq the first time it's touched in this process - otherwise a
// restart would restart every chat's seq at 1 and collide with whatever
// seq numbers are already on disk.
func (s *FSStore) nextIntentSeq(ctx context.Context, chatID string) (int64, error) {
	s.seqMu.Lock()
	defer s.seqMu.Unlock()
	if _, seeded := s.seqs[chatID]; !seeded {
		existing, err := s.ReadEntries(ctx, chatID, 0)
		if err != nil {
			return 0, err
		}
		var max int64
		for _, e := range existing {
			if e.Seq > max {
				max = e.Seq
			}
		}
		s.seqs[chatID] = max
	}
	s.seqs[chatID]++
	return s.seqs[chatID], nil
}

// ReadEntries decodes each JSONL line as an Entry, skipping lines that
// aren't one (the OTel exporter's own lines have no chat_id/kind and
// unmarshal to zero values, which this filters out).
func (s *FSStore) ReadEntries(ctx context.Context, chatID string, fromSeq int64) ([]Entry, error) {
	rc, err := s.ReadStream(ctx, chatID)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer rc.Close()

	var out []Entry
	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		var e Entry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil || e.ChatID == "" || e.Kind == "" {
			continue
		}
		if e.Seq >= fromSeq {
			out = append(out, e)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("ledger: scan %q: %w", chatID, err)
	}
	return out, nil
}
