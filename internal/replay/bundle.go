// Package replay loads a recorded ledger bundle and replays it (sequence + shallow identity matching).
package replay

import (
	"archive/zip"
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// line mirrors ledger's unexported on-disk shape.
type line struct {
	Timestamp time.Time      `json:"timestamp"`
	Attrs     map[string]any `json:"attributes"`
}

// Manifest mirrors bundle header ledger.AssembleBundle writes (manifest.json).
type Manifest struct {
	QuackVersion   string `json:"quack_version"`
	SemConvVersion string `json:"semconv_version"`
	SessionID      string `json:"session_id"`
}

// Load reads a bundle from path (ZIP from ledger.AssembleBundle or bare entries.jsonl).
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

	var manifest Manifest
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
	return buildSession(entries, manifest)
}

func loadJSONL(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("replay: open %q: %w", path, err)
	}
	defer f.Close()
	return buildSession(f, Manifest{})
}

// buildSession parses r as newline-delimited ledger entries into streams.
func buildSession(r io.Reader, manifest Manifest) (*Session, error) {
	s := &Session{manifest: manifest, streams: map[StreamKey]*streamState{}}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // a chat entry's full messages can be large
	for sc.Scan() {
		raw := strings.TrimSpace(sc.Text())
		if raw == "" {
			continue
		}
		var l line
		if err := json.Unmarshal([]byte(raw), &l); err != nil {
			return nil, fmt.Errorf("replay: parse entry: %w", err)
		}
		s.ingest(l)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("replay: read entries: %w", err)
	}
	s.finalize()
	return s, nil
}

// attrStr reads a string attribute; "" if absent or wrong type.
func attrStr(attrs map[string]any, key string) string {
	s, _ := attrs[key].(string)
	return s
}

// attrFirstOf reads first element of a JSON array attribute as string (semconv slice round-trip).
func attrFirstOf(attrs map[string]any, key string) string {
	arr, _ := attrs[key].([]any)
	if len(arr) == 0 {
		return ""
	}
	s, _ := arr[0].(string)
	return s
}

// attrInt64 reads a numeric attribute (JSON decodes numbers as float64).
func attrInt64(attrs map[string]any, key string) int64 {
	f, _ := attrs[key].(float64)
	return int64(f)
}

// attrFloat64 reads a numeric attribute as float64 (judge scores are fractional).
func attrFloat64(attrs map[string]any, key string) float64 {
	f, _ := attrs[key].(float64)
	return f
}

// sortByTime sorts entries by timestamp; stable so same-timestamp entries keep append order.
func sortByTime[T any](entries []T, tsOf func(T) time.Time) {
	sort.SliceStable(entries, func(i, j int) bool { return tsOf(entries[i]).Before(tsOf(entries[j])) })
}
