package remarkable

import (
	"context"
	"testing"
	"time"
)

func TestClientLoginAndListDocuments(t *testing.T) {
	fc := newFakeRMCloud("user@example.com", "hunter2")
	defer fc.Close()

	mod := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	fc.setDocs([]fixtureDoc{
		{ID: "doc-1", Name: "Meeting Notes", Folder: "", LastModified: mod, PDF: []byte("%PDF-1")},
		{ID: "doc-2", Name: "Scan", Folder: "Inbox/Scans", LastModified: mod, PDF: []byte("%PDF-2")},
	})

	c := newRMClient(fc.Server.URL, "user@example.com", "hunter2")
	ctx := context.Background()

	if err := c.login(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}

	docs, err := c.listDocuments(ctx)
	if err != nil {
		t.Fatalf("listDocuments: %v", err)
	}
	if len(docs) != 2 {
		t.Fatalf("got %d docs, want 2: %+v", len(docs), docs)
	}

	byID := map[string]remoteDoc{}
	for _, d := range docs {
		byID[d.ID] = d
	}
	if got := byID["doc-1"]; got.Name != "Meeting Notes" || got.Folder != "" {
		t.Errorf("doc-1 = %+v, want Name=Meeting Notes Folder=\"\"", got)
	}
	if got := byID["doc-2"]; got.Name != "Scan" || got.Folder != "Inbox/Scans" {
		t.Errorf("doc-2 = %+v, want Name=Scan Folder=Inbox/Scans", got)
	}
	if !byID["doc-1"].LastModified.Equal(mod) {
		t.Errorf("doc-1 LastModified = %v, want %v", byID["doc-1"].LastModified, mod)
	}
}

func TestClientLoginRejectsBadCredentials(t *testing.T) {
	fc := newFakeRMCloud("user@example.com", "hunter2")
	defer fc.Close()

	c := newRMClient(fc.Server.URL, "user@example.com", "wrong")
	if err := c.login(context.Background()); err == nil {
		t.Fatal("expected an error for bad credentials, got nil")
	}
}

func TestClientDownloadPDF(t *testing.T) {
	fc := newFakeRMCloud("user@example.com", "hunter2")
	defer fc.Close()
	fc.setDocs([]fixtureDoc{{ID: "doc-1", Name: "Notes", PDF: []byte("%PDF-hello")}})

	c := newRMClient(fc.Server.URL, "user@example.com", "hunter2")
	ctx := context.Background()
	if err := c.login(ctx); err != nil {
		t.Fatalf("login: %v", err)
	}

	data, err := c.downloadPDF(ctx, "doc-1")
	if err != nil {
		t.Fatalf("downloadPDF: %v", err)
	}
	if string(data) != "%PDF-hello" {
		t.Errorf("downloadPDF = %q, want %q", data, "%PDF-hello")
	}
}

func TestClientReLoginsOnExpiredToken(t *testing.T) {
	fc := newFakeRMCloud("user@example.com", "hunter2")
	defer fc.Close()
	fc.setDocs([]fixtureDoc{{ID: "doc-1", Name: "Notes"}})

	c := newRMClient(fc.Server.URL, "user@example.com", "hunter2")
	ctx := context.Background()

	// No token yet: authedRequest must log in and retry once on its own.
	if _, err := c.listDocuments(ctx); err != nil {
		t.Fatalf("listDocuments: %v", err)
	}
	if fc.loginCount != 1 {
		t.Fatalf("loginCount = %d, want 1", fc.loginCount)
	}

	// force the server to invalidate the client's token, as if it expired
	fc.mu.Lock()
	fc.token = "stale-server-side"
	fc.mu.Unlock()

	if _, err := c.listDocuments(ctx); err != nil {
		t.Fatalf("listDocuments after expiry: %v", err)
	}
	if fc.loginCount != 2 {
		t.Fatalf("loginCount after expiry = %d, want 2 (one re-login)", fc.loginCount)
	}
}
