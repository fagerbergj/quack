// Command qa-mock is a fake GitHub API and webhook sender for QA without real credentials. Subcommands:
// serve --fixtures DIR [--addr A] [--record TOKEN]; send --secret S --url U --event E --fixture F; deliveries
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/fagerbergj/quack-extensions/github"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: qa-mock <serve|send|deliveries> ...")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		runServe(os.Args[2:])
	case "send":
		runSend(os.Args[2:])
	case "deliveries":
		runDeliveries(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		os.Exit(2)
	}
}

// --- serve: fixture-backed fake api.github.com ---

// server replays fixtures keyed by method+path rather than modelling GitHub's API: a fixture
// captured once in record mode replays byte-identical offline.
type server struct {
	dir        string // fixtures directory: GET fixtures live at <dir>/get/<key>.json
	recordTok  string // if set, GET misses proxy to real GitHub and are saved
	http       *http.Client
	mu         sync.Mutex
	deliveries *os.File // append-only JSONL of every mutating call
}

func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	fixtures := fs.String("fixtures", "testdata/qa", "fixture directory")
	addr := fs.String("addr", ":8090", "listen address")
	record := fs.String("record", "", "real GitHub token; GET misses proxy+save instead of 404")
	_ = fs.Parse(args)

	if err := os.MkdirAll(filepath.Join(*fixtures, "get"), 0o755); err != nil {
		log.Fatal(err)
	}
	df, err := os.OpenFile(filepath.Join(*fixtures, "deliveries.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = df.Close() }()

	s := &server{dir: *fixtures, recordTok: *record, http: &http.Client{Timeout: 15 * time.Second}, deliveries: df}
	log.Printf("qa-mock github server on %s, fixtures=%s, record=%v", *addr, *fixtures, *record != "")
	log.Fatal(http.ListenAndServe(*addr, s))
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// App-installation-token minting: any App reaching the mock gets a fake token.
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens") {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "qa-mock-token",
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
		return
	}

	body, _ := io.ReadAll(r.Body)

	switch r.Method {
	case http.MethodGet:
		s.serveFixture(w, r)
	default:
		s.recordDelivery(r.Method, r.URL.String(), body)
		s.mockMutationResponse(w, r)
	}
}

func fixtureKey(r *http.Request) string {
	key := r.Method + "_" + r.URL.Path
	if r.URL.RawQuery != "" {
		key += "_" + r.URL.RawQuery
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8]) + ".json"
}

func (s *server) serveFixture(w http.ResponseWriter, r *http.Request) {
	path := filepath.Join(s.dir, "get", fixtureKey(r))
	if b, err := os.ReadFile(path); err == nil {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
		return
	}
	if s.recordTok == "" {
		http.Error(w, fmt.Sprintf("qa-mock: no fixture for %s %s (run with --record to capture one)", r.Method, r.URL.Path), http.StatusNotFound)
		return
	}
	real, err := http.NewRequest(r.Method, "https://api.github.com"+r.URL.RequestURI(), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	real.Header.Set("Authorization", "Bearer "+s.recordTok)
	real.Header.Set("Accept", "application/vnd.github+json")
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

// mockMutationResponse fabricates a generic success body for any write; QA asserts on the
// recorded delivery, not GitHub's response shape.
func (s *server) mockMutationResponse(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":       time.Now().UnixNano(),
		"html_url": "https://mock.invalid" + r.URL.Path,
		"node_id":  "MOCK_" + strconv.FormatInt(time.Now().UnixNano(), 36),
	})
}

func (s *server) recordDelivery(method, url string, body []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := map[string]any{
		"time":   time.Now().UTC().Format(time.RFC3339Nano),
		"method": method,
		"url":    url,
		"body":   json.RawMessage(body),
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	_, _ = s.deliveries.Write(append(b, '\n'))
}

// --- deliveries: dump the recorded mutation log ---

func runDeliveries(args []string) {
	fs := flag.NewFlagSet("deliveries", flag.ExitOnError)
	fixtures := fs.String("fixtures", "testdata/qa", "fixture directory")
	_ = fs.Parse(args)
	b, err := os.ReadFile(filepath.Join(*fixtures, "deliveries.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("(no deliveries recorded yet)")
			return
		}
		log.Fatal(err)
	}
	_, _ = os.Stdout.Write(b)
}

// --- send: build + sign a webhook from a fixture, POST it ---

func runSend(args []string) {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	fixture := fs.String("fixture", "", "path to the JSON webhook payload fixture")
	event := fs.String("event", "issues", "X-GitHub-Event value")
	url := fs.String("url", "http://localhost:8080/api/v1/github/webhook", "quack webhook endpoint")
	secret := fs.String("secret", "", "webhook secret (must match extensions.github.webhook_secret)")
	_ = fs.Parse(args)
	if *fixture == "" || *secret == "" {
		fmt.Fprintln(os.Stderr, "send requires --fixture and --secret")
		os.Exit(2)
	}
	body, err := os.ReadFile(*fixture)
	if err != nil {
		log.Fatal(err)
	}
	sig := gh.SignWebhookBody([]byte(*secret), body)

	req, err := http.NewRequest(http.MethodPost, *url, bytes.NewReader(body))
	if err != nil {
		log.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", *event)
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Delivery", fmt.Sprintf("qa-mock-%d", time.Now().UnixNano()))

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	fmt.Printf("%s %s -> %d\n%s\n", *event, *url, resp.StatusCode, b)
	if resp.StatusCode >= 300 {
		os.Exit(1)
	}
}
