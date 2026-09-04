// Package replay loads a recorded ledger bundle and replays it (sequence + shallow identity matching).
package replay

import (
	"archive/zip"
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/fagerbergj/quack/internal/ledger"
)

// Load reads a bundle from path (ZIP from ledger.AssembleBundle or bare
// entries.jsonl of ledger.Entry lines). Only ledger.LedgerVersion bundles
// are supported; older OTel-attribute bundles are rejected by their manifest.
func Load(path string) (*Session, error) {
	if strings.HasSuffix(path, ".zip") {
		return loadZip(path)
	}
	return loadJSONL(path)
}

func loadZip(path string) (*Session, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("replay: open bundle %q: %w", path, err)
	}
	defer zr.Close()

	var manifest ledger.Manifest
	var entries io.ReadCloser
	for _, f := range zr.File {
		switch f.Name {
		case "manifest.json":
			rc, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("replay: open manifest.json: %w", err)
			}
			err = json.NewDecoder(rc).Decode(&manifest)
			rc.Close()
			if err != nil {
				return nil, fmt.Errorf("replay: decode manifest.json: %w", err)
			}
		case "entries.jsonl":
			rc, err := f.Open()
			if err != nil {
				return nil, fmt.Errorf("replay: open entries.jsonl: %w", err)
			}
			entries = rc
		}
	}
	if entries == nil {
		return nil, fmt.Errorf("replay: bundle %q has no entries.jsonl", path)
	}
	defer entries.Close()
	if manifest.LedgerVersion != ledger.LedgerVersion {
		return nil, fmt.Errorf("replay: bundle %q is ledger_version %d, this build reads %d only", path, manifest.LedgerVersion, ledger.LedgerVersion)
	}
	return buildSession(entries, manifest)
}

func loadJSONL(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("replay: open %q: %w", path, err)
	}
	defer f.Close()
	return buildSession(f, ledger.Manifest{})
}

// FromStore builds a Session straight from chatID's observation entries in
// store - the same rows AssembleBundle would export, minus the ZIP.
func FromStore(ctx context.Context, store ledger.LedgerStore, chatID string) (*Session, error) {
	entries, err := ledger.ReadObservations(ctx, store, chatID)
	if err != nil {
		return nil, fmt.Errorf("replay: %w", err)
	}
	s := &Session{manifest: ledger.Manifest{LedgerVersion: ledger.LedgerVersion, SessionID: chatID}, streams: map[StreamKey]*streamState{}}
	for _, e := range entries {
		s.ingest(e)
	}
	s.finalize()
	return s, nil
}

// buildSession parses r as newline-delimited ledger entries into streams.
func buildSession(r io.Reader, manifest ledger.Manifest) (*Session, error) {
	s := &Session{manifest: manifest, streams: map[StreamKey]*streamState{}}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // a chat entry's full messages can be large
	for sc.Scan() {
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		var e ledger.Entry
		if err := json.Unmarshal([]byte(raw), &e); err != nil {
			return nil, fmt.Errorf("replay: parse entry: %w", err)
		}
		s.ingest(e)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("replay: read entries: %w", err)
	}
	s.finalize()
	return s, nil
}

// sortByTime sorts entries by timestamp; stable so same-timestamp entries keep append order.
func sortByTime[T any](entries []T, tsOf func(T) time.Time) {
	sort.SliceStable(entries, func(i, j int) bool { return tsOf(entries[i]).Before(tsOf(entries[j])) })
}
