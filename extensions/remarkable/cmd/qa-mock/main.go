// Command qa-mock fakes rmfakecloud's UI/export API for QA (see quack's docs/qa-mocks.md).
// Subcommands: serve, drop.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// rawEntry/rawTree mirror the extension's own client.go shapes exactly
// (rmfakecloud's viewmodel.Entry/DocumentTree) - see remarkable/client.go.
type rawEntry struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	LastModified time.Time  `json:"lastModified"`
	IsFolder     bool       `json:"isFolder"`
	Children     []rawEntry `json:"children"`
}

type rawTree struct {
	Entries []rawEntry `json:"Entries"`
	Trash   []rawEntry `json:"Trash"`
}

// fixtureDoc is one document, persisted to <fixtures>/docs.json so `drop`
// (a separate process invocation) can add to a running `serve`.
type fixtureDoc struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Folder       string    `json:"folder"`
	LastModified time.Time `json:"lastModified"`
	PDFPath      string    `json:"pdfPath"` // relative to fixtures dir
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: qa-mock <serve|drop> ...")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "serve":
		runServe(os.Args[2:])
	case "drop":
		runDrop(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[1])
		os.Exit(2)
	}
}

func docsFile(fixtures string) string { return filepath.Join(fixtures, "docs.json") }

func loadDocs(fixtures string) ([]fixtureDoc, error) {
	b, err := os.ReadFile(docsFile(fixtures))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var docs []fixtureDoc
	if err := json.Unmarshal(b, &docs); err != nil {
		return nil, err
	}
	return docs, nil
}

func saveDocs(fixtures string, docs []fixtureDoc) error {
	b, err := json.MarshalIndent(docs, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(docsFile(fixtures), b, 0o644)
}

// --- drop: add a new document fixture to a running serve ---

func runDrop(args []string) {
	fs := flag.NewFlagSet("drop", flag.ExitOnError)
	fixtures := fs.String("fixtures", "testdata/qa", "fixture directory")
	name := fs.String("name", "", "document visible name, e.g. \"2-page note\"")
	folder := fs.String("folder", "", "folder path, e.g. inbox")
	pdfPath := fs.String("pdf", "", "path to a PDF file to serve as this document's content")
	_ = fs.Parse(args) // fs is ExitOnError

	if *name == "" || *pdfPath == "" {
		fmt.Fprintln(os.Stderr, "drop requires --name and --pdf")
		os.Exit(2)
	}
	if err := os.MkdirAll(*fixtures, 0o755); err != nil {
		log.Fatal(err)
	}
	docs, err := loadDocs(*fixtures)
	if err != nil {
		log.Fatal(err)
	}
	id := randID()
	stored := filepath.Join("blobs", id+".pdf")
	if err := os.MkdirAll(filepath.Join(*fixtures, "blobs"), 0o755); err != nil {
		log.Fatal(err)
	}
	pdf, err := os.ReadFile(*pdfPath)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(*fixtures, stored), pdf, 0o644); err != nil {
		log.Fatal(err)
	}
	docs = append(docs, fixtureDoc{ID: id, Name: *name, Folder: *folder, LastModified: time.Now(), PDFPath: stored})
	if err := saveDocs(*fixtures, docs); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("dropped document %s (%q) into %s\n", id, *name, *fixtures)
}

func randID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// --- serve: the fake rmfakecloud UI/export API ---

type server struct {
	fixtures string
	email    string
	password string

	mu    sync.Mutex
	token string
}

func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	fixtures := fs.String("fixtures", "testdata/qa", "fixture directory")
	addr := fs.String("addr", ":8091", "listen address")
	email := fs.String("email", "qa@example.com", "login email the extension must be configured with")
	password := fs.String("password", "qa-password", "login password the extension must be configured with")
	_ = fs.Parse(args) // fs is ExitOnError

	if err := os.MkdirAll(*fixtures, 0o755); err != nil {
		log.Fatal(err)
	}
	s := &server{fixtures: *fixtures, email: *email, password: *password}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ui/api/login", s.handleLogin)
	mux.HandleFunc("GET /ui/api/documents", s.handleDocuments)
	mux.HandleFunc("GET /ui/api/documents/{id}", s.handleDownload)
	log.Printf("qa-mock remarkable server on %s, fixtures=%s", *addr, *fixtures)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var form struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&form); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if form.Email != s.email || form.Password != s.password {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	s.mu.Lock()
	s.token = "qa-mock-token-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	tok := s.token
	s.mu.Unlock()
	_, _ = w.Write([]byte(tok))
}

func (s *server) authOK(r *http.Request) bool {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.mu.Lock()
	defer s.mu.Unlock()
	return tok != "" && tok == s.token
}

func (s *server) handleDocuments(w http.ResponseWriter, r *http.Request) {
	if !s.authOK(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	docs, err := loadDocs(s.fixtures)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(buildTree(docs))
}

func (s *server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if !s.authOK(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	docs, err := loadDocs(s.fixtures)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	id := r.PathValue("id")
	for _, d := range docs {
		if d.ID == id {
			pdf, err := os.ReadFile(filepath.Join(s.fixtures, d.PDFPath))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(pdf)
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
}

// buildTree groups flat fixtureDocs into the nested rawTree shape
// rmfakecloud's GET /ui/api/documents actually returns.
func buildTree(docs []fixtureDoc) rawTree {
	type node struct {
		dirs map[string]*node
		docs []rawEntry
	}
	root := &node{dirs: map[string]*node{}}
	for _, d := range docs {
		cur := root
		if d.Folder != "" {
			for _, seg := range strings.Split(d.Folder, "/") {
				child, ok := cur.dirs[seg]
				if !ok {
					child = &node{dirs: map[string]*node{}}
					cur.dirs[seg] = child
				}
				cur = child
			}
		}
		cur.docs = append(cur.docs, rawEntry{ID: d.ID, Name: d.Name, LastModified: d.LastModified})
	}
	var build func(n *node) []rawEntry
	build = func(n *node) []rawEntry {
		var out []rawEntry
		for name, child := range n.dirs {
			out = append(out, rawEntry{Name: name, IsFolder: true, Children: build(child)})
		}
		out = append(out, n.docs...)
		return out
	}
	return rawTree{Entries: build(root)}
}
