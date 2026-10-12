package remarkable

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// remoteDoc is one document as seen through rmfakecloud's UI API document
// tree, flattened with its folder path resolved.
type remoteDoc struct {
	ID           string
	Name         string // visibleName
	Folder       string // "" at root, else "Parent/Child" joined by name
	LastModified time.Time
}

// rawEntry mirrors rmfakecloud's viewmodel.Entry union as it serializes; a Document has no
// "isFolder" key, so it decodes false.
type rawEntry struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	LastModified time.Time  `json:"lastModified"`
	IsFolder     bool       `json:"isFolder"`
	Children     []rawEntry `json:"children"`
}

// rawTree mirrors viewmodel.DocumentTree, which has no json tags of its own
// - the field names serialize as-is ("Entries"/"Trash").
type rawTree struct {
	Entries []rawEntry `json:"Entries"`
}

// rmClient talks to rmfakecloud's UI/export API, the only surface with flat metadata and PDFs.
// Login yields a 24h bearer JWT; authedRequest re-logs in once on a 401.
type rmClient struct {
	baseURL    string
	email      string
	password   string
	httpClient *http.Client

	mu    sync.Mutex
	token string
}

func newRMClient(baseURL, email, password string) *rmClient {
	return &rmClient{
		baseURL:    strings.TrimRight(baseURL, "/"),
		email:      email,
		password:   password,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (c *rmClient) login(ctx context.Context) error {
	body, err := json.Marshal(loginRequest{Email: c.email, Password: c.password})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/ui/api/login", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("connect to rmfakecloud: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	tok, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read login response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("rmfakecloud login: status %d: %s", resp.StatusCode, strings.TrimSpace(string(tok)))
	}

	c.mu.Lock()
	c.token = strings.TrimSpace(string(tok))
	c.mu.Unlock()
	return nil
}

// authedRequest issues one GET, re-logging in exactly once on a 401 (the
// token expired or was never obtained) before giving up.
func (c *rmClient) authedRequest(ctx context.Context, path string) (*http.Response, error) {
	do := func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		tok := c.token
		c.mu.Unlock()
		req.Header.Set("Authorization", "Bearer "+tok)
		return c.httpClient.Do(req)
	}

	resp, err := do()
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		_ = resp.Body.Close()
		if err := c.login(ctx); err != nil {
			return nil, err
		}
		resp, err = do()
		if err != nil {
			return nil, err
		}
	}
	return resp, nil
}

// listDocuments fetches and flattens the document tree. Trash is excluded -
// a deleted document should stop being polled, not re-dispatch.
func (c *rmClient) listDocuments(ctx context.Context) ([]remoteDoc, error) {
	resp, err := c.authedRequest(ctx, "/ui/api/documents")
	if err != nil {
		return nil, fmt.Errorf("list documents: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("list documents: status %d: %s", resp.StatusCode, string(b))
	}

	var tree rawTree
	if err := json.NewDecoder(resp.Body).Decode(&tree); err != nil {
		return nil, fmt.Errorf("decode document tree: %w", err)
	}

	var out []remoteDoc
	walkEntries(tree.Entries, "", &out)
	return out, nil
}

func walkEntries(entries []rawEntry, folder string, out *[]remoteDoc) {
	for _, e := range entries {
		if e.IsFolder {
			childFolder := e.Name
			if folder != "" {
				childFolder = folder + "/" + e.Name
			}
			walkEntries(e.Children, childFolder, out)
			continue
		}
		*out = append(*out, remoteDoc{
			ID:           e.ID,
			Name:         e.Name,
			Folder:       folder,
			LastModified: e.LastModified,
		})
	}
}

// downloadPDF exports a document as PDF via rmfakecloud's on-demand
// renderer (type=pdf works for any source type: notebook, pdf, epub).
func (c *rmClient) downloadPDF(ctx context.Context, docID string) ([]byte, error) {
	resp, err := c.authedRequest(ctx, "/ui/api/documents/"+url.PathEscape(docID)+"?type=pdf")
	if err != nil {
		return nil, fmt.Errorf("download document %s: %w", docID, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("download document %s: status %d: %s", docID, resp.StatusCode, string(b))
	}
	return io.ReadAll(resp.Body)
}
