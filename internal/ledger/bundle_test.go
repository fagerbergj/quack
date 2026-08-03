package ledger

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"
)

// TestAssembleBundleRoundTrip writes entries via FSStore, assembles a
// bundle, unzips it, and asserts the manifest fields and that entries.jsonl
// comes back byte-identical to what was appended.
func TestAssembleBundleRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFSStore(dir)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	ctx := context.Background()
	lines := []string{`{"seq":1}`, `{"seq":2}`, `{"seq":3}`}
	for _, l := range lines {
		if err := s.Append(ctx, "chat-1", []byte(l)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	want, err := io.ReadAll(mustReadStream(t, s, "chat-1"))
	if err != nil {
		t.Fatalf("read want: %v", err)
	}

	entries := mustReadStream(t, s, "chat-1")
	var buf bytes.Buffer
	if err := AssembleBundle(ctx, s, "chat-1", "v1.2.3", "1.41.0", entries, &buf); err != nil {
		t.Fatalf("AssembleBundle: %v", err)
	}

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	files := map[string]*zip.File{}
	for _, f := range zr.File {
		files[f.Name] = f
	}
	if _, ok := files["manifest.json"]; !ok {
		t.Fatalf("bundle missing manifest.json; got %v", filenames(zr))
	}
	if _, ok := files["entries.jsonl"]; !ok {
		t.Fatalf("bundle missing entries.jsonl; got %v", filenames(zr))
	}
	if _, ok := files["clone.bundle"]; ok {
		t.Errorf("bundle has clone.bundle but none was recorded")
	}

	mf, err := files["manifest.json"].Open()
	if err != nil {
		t.Fatalf("open manifest.json: %v", err)
	}
	defer mf.Close()
	var m Manifest
	if err := json.NewDecoder(mf).Decode(&m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if m.QuackVersion != "v1.2.3" {
		t.Errorf("QuackVersion = %q, want v1.2.3", m.QuackVersion)
	}
	if m.SemConvVersion != "1.41.0" {
		t.Errorf("SemConvVersion = %q, want 1.41.0", m.SemConvVersion)
	}
	if m.SessionID != "chat-1" {
		t.Errorf("SessionID = %q, want chat-1", m.SessionID)
	}
	if m.CloneSnapshot {
		t.Errorf("CloneSnapshot = true, want false (none recorded)")
	}
	if m.ExportedAt.IsZero() {
		t.Errorf("ExportedAt is zero")
	}

	ef, err := files["entries.jsonl"].Open()
	if err != nil {
		t.Fatalf("open entries.jsonl: %v", err)
	}
	defer ef.Close()
	got, err := io.ReadAll(ef)
	if err != nil {
		t.Fatalf("read entries.jsonl: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("entries.jsonl mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

// TestAssembleBundleWithCloneSnapshot checks clone.bundle is included and
// the manifest flag set when the store implements CloneSnapshotReader and
// has a snapshot for the session.
func TestAssembleBundleWithCloneSnapshot(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFSStore(dir)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	ctx := context.Background()
	if err := s.Append(ctx, "chat-2", []byte(`{"seq":1}`)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	store := &fakeCloneStore{FSStore: s, snapshot: []byte("PACK-DATA")}

	entries := mustReadStream(t, store, "chat-2")
	var buf bytes.Buffer
	if err := AssembleBundle(ctx, store, "chat-2", "dev", "1.41.0", entries, &buf); err != nil {
		t.Fatalf("AssembleBundle: %v", err)
	}

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	var cloneFile *zip.File
	for _, f := range zr.File {
		if f.Name == "clone.bundle" {
			cloneFile = f
		}
	}
	if cloneFile == nil {
		t.Fatalf("bundle missing clone.bundle; got %v", filenames(zr))
	}
	rc, err := cloneFile.Open()
	if err != nil {
		t.Fatalf("open clone.bundle: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read clone.bundle: %v", err)
	}
	if string(got) != "PACK-DATA" {
		t.Errorf("clone.bundle = %q, want PACK-DATA", got)
	}
}

func filenames(zr *zip.Reader) []string {
	out := make([]string, len(zr.File))
	for i, f := range zr.File {
		out[i] = f.Name
	}
	return out
}

func mustReadStream(t *testing.T, s LedgerStore, sessionID string) io.Reader {
	t.Helper()
	rc, err := s.ReadStream(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("ReadStream(%s): %v", sessionID, err)
	}
	t.Cleanup(func() { _ = rc.Close() })
	return rc
}

// fakeCloneStore wraps FSStore to add a CloneSnapshotReader for one fixed
// session, without changing FSStore itself (clone-snapshot creation is out
// of scope for this milestone - see .quack/replay-log.md).
type fakeCloneStore struct {
	*FSStore
	snapshot []byte
}

func (f *fakeCloneStore) ReadCloneSnapshot(_ context.Context, sessionID string) (io.ReadCloser, bool, error) {
	if sessionID != "chat-2" {
		return nil, false, nil
	}
	return io.NopCloser(bytes.NewReader(f.snapshot)), true, nil
}
