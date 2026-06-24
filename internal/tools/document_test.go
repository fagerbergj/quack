package tools

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/docstore"
)

// fakeDocStore is an in-memory DocStore for testing the tool logic without a DB.
type fakeDocStore struct {
	byID   map[string]docstore.Document
	byHash map[string]docstore.Document
}

func newFakeDocStore() *fakeDocStore {
	return &fakeDocStore{byID: map[string]docstore.Document{}, byHash: map[string]docstore.Document{}}
}

func (f *fakeDocStore) Create(_ context.Context, d docstore.Document) error {
	f.byID[d.ID] = d
	if d.ContentHash != "" {
		f.byHash[d.ContentHash] = d
	}
	return nil
}
func (f *fakeDocStore) Get(_ context.Context, id string) (docstore.Document, bool, error) {
	d, ok := f.byID[id]
	return d, ok, nil
}
func (f *fakeDocStore) GetByHash(_ context.Context, h string) (docstore.Document, bool, error) {
	d, ok := f.byHash[h]
	return d, ok, nil
}
func (f *fakeDocStore) Update(_ context.Context, d docstore.Document) error {
	f.byID[d.ID] = d
	return nil
}

// fakeFTS is an in-memory FTSIndex: substring match over title/summary/content.
type fakeFTS struct{ docs map[string]docstore.Document }

func newFakeFTS() *fakeFTS { return &fakeFTS{docs: map[string]docstore.Document{}} }

func (f *fakeFTS) Index(_ context.Context, d docstore.Document) error {
	f.docs[d.ID] = d
	return nil
}
func (f *fakeFTS) Search(_ context.Context, query string, _ int) ([]docstore.FTSHit, error) {
	var out []docstore.FTSHit
	for _, d := range f.docs {
		if strings.Contains(d.Title+" "+d.Summary+" "+d.Content, query) {
			out = append(out, docstore.FTSHit{ID: d.ID, Title: d.Title, Summary: d.Summary})
		}
	}
	return out, nil
}

// fakeVector is an in-memory VectorIndex: stores content per doc, substring match.
type fakeVector struct{ byDoc map[string]string }

func newFakeVector() *fakeVector { return &fakeVector{byDoc: map[string]string{}} }

func (f *fakeVector) Index(_ context.Context, docID, content string) error {
	f.byDoc[docID] = content
	return nil
}
func (f *fakeVector) Search(_ context.Context, query string, _ int) ([]docstore.VectorHit, error) {
	var out []docstore.VectorHit
	for id, c := range f.byDoc {
		if strings.Contains(c, query) {
			out = append(out, docstore.VectorHit{DocID: id, Chunk: c, Score: 1})
		}
	}
	return out, nil
}

func TestDocToolsRequireStore(t *testing.T) {
	for _, name := range []string{"load_document", "create_document", "update_document"} {
		if _, err := Build([]string{name}, Deps{}); err == nil {
			t.Errorf("%s should refuse to build without a DocStore", name)
		}
		if _, err := Build([]string{name}, Deps{DocStore: newFakeDocStore()}); err != nil {
			t.Errorf("%s should build with a DocStore: %v", name, err)
		}
	}
}

func TestCreateAndLoadDocument(t *testing.T) {
	store := newFakeDocStore()
	ctx := context.Background()

	res, err := createDoc(ctx, store, nil, nil, createDocArgs{Title: "T", Content: "body", Summary: "s", Tags: []string{"a"}, ContentHash: "h1"})
	if err != nil || res.ID == "" {
		t.Fatalf("create: id=%q err=%v", res.ID, err)
	}
	got, err := loadDoc(ctx, store, loadDocArgs{ID: res.ID})
	if err != nil || got.Title != "T" || got.Content != "body" || !slices.Equal(got.Tags, []string{"a"}) {
		t.Fatalf("load: %+v err=%v", got, err)
	}

	// Dedup: same content_hash returns the existing id, no new record.
	res2, err := createDoc(ctx, store, nil, nil, createDocArgs{Title: "other", Content: "body2", ContentHash: "h1"})
	if err != nil || res2.ID != res.ID {
		t.Errorf("dedup: got id=%q err=%v, want existing %q", res2.ID, err, res.ID)
	}

	// Empty content is rejected.
	if _, err := createDoc(ctx, store, nil, nil, createDocArgs{Content: "  "}); err == nil {
		t.Error("empty content should error")
	}
	// Missing id load errors.
	if _, err := loadDoc(ctx, store, loadDocArgs{ID: "nope"}); err == nil {
		t.Error("missing id should error")
	}
}

// create_document mirrors into the FTS index, and search_document finds it.
func TestCreateIndexesToFTSAndSearch(t *testing.T) {
	store, fts := newFakeDocStore(), newFakeFTS()
	ctx := context.Background()
	res, err := createDoc(ctx, store, fts, nil, createDocArgs{Title: "Mistral release notes", Content: "body", ContentHash: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := fts.docs[res.ID]; !ok {
		t.Fatal("create_document did not index into FTS")
	}
	hits, err := fts.Search(ctx, "Mistral", 10)
	if err != nil || len(hits) != 1 || hits[0].ID != res.ID {
		t.Errorf("search: hits=%+v err=%v", hits, err)
	}
}

func TestSearchDocumentRequiresFTS(t *testing.T) {
	if _, err := Build([]string{"search_document"}, Deps{}); err == nil {
		t.Error("search_document should refuse to build without an FTS index")
	}
	if _, err := Build([]string{"search_document"}, Deps{FTS: newFakeFTS()}); err != nil {
		t.Errorf("search_document should build with an FTS index: %v", err)
	}
}

// create_document also indexes into the vector index, and semantic search finds it.
func TestCreateIndexesToVector(t *testing.T) {
	store, vec := newFakeDocStore(), newFakeVector()
	ctx := context.Background()
	res, err := createDoc(ctx, store, nil, vec, createDocArgs{Title: "t", Content: "qwen release notes", ContentHash: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := vec.byDoc[res.ID]; !ok {
		t.Fatal("create_document did not index into the vector index")
	}
	hits, err := vec.Search(ctx, "qwen", 5)
	if err != nil || len(hits) != 1 || hits[0].DocID != res.ID {
		t.Errorf("semantic search: hits=%+v err=%v", hits, err)
	}
}

func TestSemanticSearchRequiresVector(t *testing.T) {
	if _, err := Build([]string{"semantic_search_document"}, Deps{}); err == nil {
		t.Error("semantic_search_document should refuse to build without a vector index")
	}
	if _, err := Build([]string{"semantic_search_document"}, Deps{Vector: newFakeVector()}); err != nil {
		t.Errorf("semantic_search_document should build with a vector index: %v", err)
	}
}

// With no content_hash supplied, create dedups on a hash of the content — so a
// re-invoked write (e.g. a gate re-run) returns the same record, not a duplicate.
func TestCreateDedupsOnContentByDefault(t *testing.T) {
	store := newFakeDocStore()
	ctx := context.Background()
	r1, err := createDoc(ctx, store, nil, nil, createDocArgs{Content: "same body", Title: "a"})
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := createDoc(ctx, store, nil, nil, createDocArgs{Content: "same body", Title: "b"})
	if r1.ID != r2.ID {
		t.Errorf("identical content should dedup: %q vs %q", r1.ID, r2.ID)
	}
	if r3, _ := createDoc(ctx, store, nil, nil, createDocArgs{Content: "different body"}); r3.ID == r1.ID {
		t.Error("different content should not dedup")
	}
}

func TestUpdateDocumentOverlays(t *testing.T) {
	store := newFakeDocStore()
	ctx := context.Background()
	res, _ := createDoc(ctx, store, nil, nil, createDocArgs{Title: "old", Content: "body", Summary: "keep", ContentHash: "h"})

	// Only Title provided ⇒ Summary/Content unchanged.
	if _, err := updateDoc(ctx, store, updateDocArgs{ID: res.ID, Title: "new"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, _ := loadDoc(ctx, store, loadDocArgs{ID: res.ID})
	if got.Title != "new" || got.Summary != "keep" || got.Content != "body" {
		t.Errorf("overlay wrong: %+v", got)
	}
	// Updating a missing id errors.
	if _, err := updateDoc(ctx, store, updateDocArgs{ID: "nope", Title: "x"}); err == nil {
		t.Error("update missing id should error")
	}
}
