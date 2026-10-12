package remarkable

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

// fixtureDoc is one document the fake rmfakecloud instance serves.
type fixtureDoc struct {
	ID           string
	Name         string
	Folder       string // "" root, or "A/B"
	LastModified time.Time
	PDF          []byte
}

// fakeRMCloud is an httptest stand-in for rmfakecloud's UI/export API.
type fakeRMCloud struct {
	email    string
	password string

	mu         sync.Mutex
	docs       []fixtureDoc
	failPDF    map[string]bool
	token      string
	tokenSeq   int
	loginCount int

	Server *httptest.Server
}

func newFakeRMCloud(email, password string) *fakeRMCloud {
	fc := &fakeRMCloud{email: email, password: password}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /ui/api/login", fc.handleLogin)
	mux.HandleFunc("GET /ui/api/documents", fc.handleDocuments)
	mux.HandleFunc("GET /ui/api/documents/{id}", fc.handleDownload)
	fc.Server = httptest.NewServer(mux)
	return fc
}

func (fc *fakeRMCloud) Close() { fc.Server.Close() }

func (fc *fakeRMCloud) setDocs(docs []fixtureDoc) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.docs = docs
}

// failDownloads makes the PDF export fail for these doc IDs while they stay
// listed - a document that exports badly, not one that vanished.
func (fc *fakeRMCloud) failDownloads(ids ...string) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	fc.failPDF = map[string]bool{}
	for _, id := range ids {
		fc.failPDF[id] = true
	}
}

func (fc *fakeRMCloud) handleLogin(w http.ResponseWriter, r *http.Request) {
	var form loginRequest
	if err := json.NewDecoder(r.Body).Decode(&form); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if form.Email != fc.email || form.Password != fc.password {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	fc.tokenSeq++
	fc.token = "token-" + strconv.Itoa(fc.tokenSeq)
	fc.loginCount++
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(fc.token))
}

func (fc *fakeRMCloud) authOK(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	tok := strings.TrimPrefix(auth, "Bearer ")
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return tok != "" && tok == fc.token
}

func (fc *fakeRMCloud) handleDocuments(w http.ResponseWriter, r *http.Request) {
	if !fc.authOK(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	fc.mu.Lock()
	tree := buildTree(fc.docs)
	fc.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(tree)
}

func (fc *fakeRMCloud) handleDownload(w http.ResponseWriter, r *http.Request) {
	if !fc.authOK(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	id := r.PathValue("id")
	fc.mu.Lock()
	if fc.failPDF[id] {
		fc.mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	var pdf []byte
	found := false
	for _, d := range fc.docs {
		if d.ID == id {
			pdf = d.PDF
			found = true
			break
		}
	}
	fc.mu.Unlock()
	if !found {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(pdf)
}

// buildTree groups flat fixtureDocs into the nested Directory/Document JSON
// shape rmfakecloud's GET /ui/api/documents actually returns.
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
