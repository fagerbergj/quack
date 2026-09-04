package ledger

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// LedgerVersion is the bundle entry shape; bump it whenever Entry or a
// payload struct changes incompatibly. Version 2 is the typed-Entry shape;
// version 1 (raw OTel attribute lines) bundles are unsupported.
const LedgerVersion = 2

// Manifest is the bundle's self-describing header - everything a reader with
// zero access to quack's DB needs to know what produced the bundle.
type Manifest struct {
	QuackVersion  string    `json:"quack_version"`
	LedgerVersion int       `json:"ledger_version"`
	SessionID     string    `json:"session_id"`
	ExportedAt    time.Time `json:"exported_at"`
}

// ErrNoRecording is returned by ReadObservations for a chat with no
// observation entries (never ran, recording off, or hard-deleted).
var ErrNoRecording = errors.New("ledger: no recording for this chat")

// ReadObservations returns chatID's observation entries in seq order.
func ReadObservations(ctx context.Context, store LedgerStore, chatID string) ([]Entry, error) {
	all, err := store.ReadEntries(ctx, chatID, 0)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, e := range all {
		if IsObservation(e.Kind) {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		return nil, ErrNoRecording
	}
	return out, nil
}

// AssembleBundle streams sessionID's recording as a ZIP to w: manifest.json
// plus entries.jsonl (one Entry per line, seq order).
func AssembleBundle(ctx context.Context, store LedgerStore, sessionID, quackVersion string, w io.Writer) error {
	entries, err := ReadObservations(ctx, store, sessionID)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(w)
	mf, err := zw.Create("manifest.json")
	if err != nil {
		return fmt.Errorf("ledger: create manifest.json: %w", err)
	}
	if err := json.NewEncoder(mf).Encode(Manifest{QuackVersion: quackVersion, LedgerVersion: LedgerVersion, SessionID: sessionID, ExportedAt: time.Now().UTC()}); err != nil {
		return fmt.Errorf("ledger: encode manifest.json: %w", err)
	}
	ef, err := zw.Create("entries.jsonl")
	if err != nil {
		return fmt.Errorf("ledger: create entries.jsonl: %w", err)
	}
	enc := json.NewEncoder(ef)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			return fmt.Errorf("ledger: encode entry seq %d: %w", e.Seq, err)
		}
	}
	return zw.Close()
}
