package ledger

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// Manifest is the bundle's self-describing header - everything a reader with
// zero access to quack's DB or workspace needs to know what produced the
// bundle and whether anything is missing.
type Manifest struct {
	QuackVersion   string    `json:"quack_version"`
	SemConvVersion string    `json:"semconv_version"`
	SessionID      string    `json:"session_id"`
	ExportedAt     time.Time `json:"exported_at"`
	CloneSnapshot  bool      `json:"clone_snapshot"`
}

// CloneSnapshotReader is implemented by a LedgerStore that can additionally
// serve an optional git-bundle snapshot recorded alongside a session's
// entries (FSStore today). Kept separate from LedgerStore itself - clone
// snapshots are opt-in per run (config.RecordingConfig.CloneSnapshot), so
// most sessions, and stores that never produce one, never implement this.
type CloneSnapshotReader interface {
	// ReadCloneSnapshot returns sessionID's clone bundle, or ok=false if none
	// was recorded.
	ReadCloneSnapshot(ctx context.Context, sessionID string) (rc io.ReadCloser, ok bool, err error)
}

// AssembleBundle streams sessionID's recording as a ZIP to w: manifest.json,
// entries.jsonl (copied from entries - the caller's already-open
// LedgerStore.ReadStream result, never buffered whole in memory here), and
// clone.bundle when store implements CloneSnapshotReader and has one for
// this session. quackVersion/semconvVersion are the caller's build/schema
// stamps - the bundle is meant to be openable with no access back to quack's
// DB or workspace, so both travel with it instead of being inferred later.
func AssembleBundle(ctx context.Context, store LedgerStore, sessionID, quackVersion, semconvVersion string, entries io.Reader, w io.Writer) error {
	var cloneRC io.ReadCloser
	hasClone := false
	if csr, ok := store.(CloneSnapshotReader); ok {
		rc, ok2, err := csr.ReadCloneSnapshot(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("ledger: clone snapshot: %w", err)
		}
		if ok2 {
			cloneRC, hasClone = rc, true
			defer cloneRC.Close()
		}
	}

	zw := zip.NewWriter(w)

	mf, err := zw.Create("manifest.json")
	if err != nil {
		return fmt.Errorf("ledger: create manifest.json: %w", err)
	}
	manifest := Manifest{
		QuackVersion:   quackVersion,
		SemConvVersion: semconvVersion,
		SessionID:      sessionID,
		ExportedAt:     time.Now().UTC(),
		CloneSnapshot:  hasClone,
	}
	if err := json.NewEncoder(mf).Encode(manifest); err != nil {
		return fmt.Errorf("ledger: encode manifest.json: %w", err)
	}

	ef, err := zw.Create("entries.jsonl")
	if err != nil {
		return fmt.Errorf("ledger: create entries.jsonl: %w", err)
	}
	if _, err := io.Copy(ef, entries); err != nil {
		return fmt.Errorf("ledger: copy entries.jsonl: %w", err)
	}

	if hasClone {
		cf, err := zw.Create("clone.bundle")
		if err != nil {
			return fmt.Errorf("ledger: create clone.bundle: %w", err)
		}
		if _, err := io.Copy(cf, cloneRC); err != nil {
			return fmt.Errorf("ledger: copy clone.bundle: %w", err)
		}
	}

	return zw.Close()
}
