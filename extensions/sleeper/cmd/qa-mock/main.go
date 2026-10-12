// Command qa-mock is a fake api.sleeper.app that replays recorded fixtures for QA and tests.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func main() {
	fixtures := flag.String("fixtures", "testdata/get", "fixture directory")
	addr := flag.String("addr", ":8092", "listen address")
	record := flag.Bool("record", false, "proxy GET misses to the real Sleeper API and save them")
	flag.Parse()

	s, err := newServer(*fixtures, *record)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("qa-mock sleeper server on %s, fixtures=%s, record=%v", *addr, *fixtures, *record)
	log.Fatal(http.ListenAndServe(*addr, s))
}

func newServer(fixtures string, record bool) (*server, error) {
	if err := os.MkdirAll(fixtures, 0o755); err != nil {
		return nil, err
	}
	return &server{dir: fixtures, record: record, http: &http.Client{Timeout: 15 * time.Second}}, nil
}

// server replays fixtures keyed by request path+query, like github/cmd/qa-mock.
type server struct {
	dir    string
	record bool
	http   *http.Client
}

// fixtureKey mirrors github/cmd/qa-mock's key function exactly; recorded fixture names depend on it.
func fixtureKey(r *http.Request) string {
	key := r.Method + "_" + r.URL.Path
	if r.URL.RawQuery != "" {
		key += "_" + r.URL.RawQuery
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8]) + ".json"
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "sleeper API is read-only; qa-mock only serves GET", http.StatusMethodNotAllowed)
		return
	}
	path := filepath.Join(s.dir, fixtureKey(r))
	if b, err := os.ReadFile(path); err == nil {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
		return
	}
	if !s.record {
		http.Error(w, fmt.Sprintf("qa-mock: no fixture for GET %s (run with --record to capture one)", r.URL.RequestURI()), http.StatusNotFound)
		return
	}
	real, err := http.NewRequest(http.MethodGet, "https://api.sleeper.app"+r.URL.RequestURI(), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	resp, err := s.http.Do(real)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 300 {
		_ = os.WriteFile(path, b, 0o644)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(b)
}
