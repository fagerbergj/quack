package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/fagerbergj/quack/internal/docstore"
)

// Document tools are the record side of the document store (load/create/update).
// Search lives in the FTS / vector tools. All three need a DocStore in Deps; a
// nil store means documents aren't configured, so the tool refuses to build.

type loadDocArgs struct {
	ID string `json:"id"`
}

func newLoadDocument(d Deps) (tool.Tool, error) {
	if d.DocStore == nil {
		return nil, fmt.Errorf("load_document requires a document store")
	}
	return functiontool.New[loadDocArgs, docstore.Document](
		functiontool.Config{
			Name: "load_document",
			Description: "Tool to fetch a stored document by id. Use when you need a document's full text or " +
				"metadata (title, summary, tags). Returns the document record, or an error if no such id exists.",
		},
		func(tc agent.ToolContext, a loadDocArgs) (docstore.Document, error) {
			return loadDoc(tc, d.DocStore, a)
		},
	)
}

func loadDoc(ctx context.Context, store docstore.DocStore, a loadDocArgs) (docstore.Document, error) {
	doc, ok, err := store.Get(ctx, strings.TrimSpace(a.ID))
	if err != nil {
		return docstore.Document{}, err
	}
	if !ok {
		return docstore.Document{}, fmt.Errorf("load_document: no document with id %q", a.ID)
	}
	return doc, nil
}

type createDocArgs struct {
	Title       string   `json:"title"`
	Content     string   `json:"content"`
	Summary     string   `json:"summary"`
	Tags        []string `json:"tags"`
	Series      string   `json:"series"`
	DateMonth   string   `json:"date_month"`
	SourceRef   string   `json:"source_ref"`
	ContentHash string   `json:"content_hash"`
}

type docIDResult struct {
	ID string `json:"id"`
}

func newCreateDocument(d Deps) (tool.Tool, error) {
	if d.DocStore == nil {
		return nil, fmt.Errorf("create_document requires a document store")
	}
	return functiontool.New[createDocArgs, docIDResult](
		functiontool.Config{
			Name: "create_document",
			Description: "Tool to persist a new document record after extraction, cleanup, and classification. " +
				"Returns {id}. Idempotent on content_hash: if a document with the same content_hash already " +
				"exists, its id is returned instead of creating a duplicate.",
		},
		func(tc agent.ToolContext, a createDocArgs) (docIDResult, error) {
			return createDoc(tc, d.DocStore, d.FTS, d.Vector, a)
		},
	)
}

// createDoc persists the record, then mirrors it into the full-text index and the
// vector index when each is configured (so search_document / semantic_search_
// document can find it). fts and vec may be nil.
func createDoc(ctx context.Context, store docstore.DocStore, fts docstore.FTSIndex, vec docstore.VectorIndex, a createDocArgs) (docIDResult, error) {
	if strings.TrimSpace(a.Content) == "" {
		return docIDResult{}, fmt.Errorf("create_document: content is empty")
	}
	// Default the dedup key to a hash of the content when the caller didn't supply
	// one (the chat path has no source-file hash). This makes create idempotent:
	// the same content always resolves to one record — so a retried or re-invoked
	// call returns the existing id instead of inserting a duplicate.
	hash := a.ContentHash
	if hash == "" {
		sum := sha256.Sum256([]byte(a.Content))
		hash = hex.EncodeToString(sum[:])
	}
	if existing, ok, err := store.GetByHash(ctx, hash); err != nil {
		return docIDResult{}, err
	} else if ok {
		return docIDResult{ID: existing.ID}, nil // dedup
	}
	doc := docstore.Document{
		ID: uuid.NewString(), ContentHash: hash,
		Title: a.Title, Content: a.Content, Summary: a.Summary, Tags: a.Tags,
		Series: a.Series, DateMonth: a.DateMonth, SourceRef: a.SourceRef,
	}
	if err := store.Create(ctx, doc); err != nil {
		return docIDResult{}, err
	}
	if fts != nil {
		if err := fts.Index(ctx, doc); err != nil {
			return docIDResult{}, fmt.Errorf("create_document: indexed record but FTS failed: %w", err)
		}
	}
	if vec != nil {
		if err := vec.Index(ctx, doc.ID, doc.Content); err != nil {
			return docIDResult{}, fmt.Errorf("create_document: indexed record but vector index failed: %w", err)
		}
	}
	return docIDResult{ID: doc.ID}, nil
}

type searchDocArgs struct {
	Query string `json:"query"`
	Size  int    `json:"size"`
}

type searchDocResult struct {
	Results []docstore.FTSHit `json:"results"`
}

func newSearchDocument(d Deps) (tool.Tool, error) {
	if d.FTS == nil {
		return nil, fmt.Errorf("search_document requires a full-text index")
	}
	return functiontool.New[searchDocArgs, searchDocResult](
		functiontool.Config{
			Name: "search_document",
			Description: "Tool to keyword-search stored documents by title, summary, content, and tags. " +
				"Returns {results: [{id, title, summary}]}, best match first; use load_document with an id " +
				"to read the full record. For meaning-based search use semantic_search_document instead.",
		},
		func(tc agent.ToolContext, a searchDocArgs) (searchDocResult, error) {
			hits, err := d.FTS.Search(tc, strings.TrimSpace(a.Query), a.Size)
			if err != nil {
				return searchDocResult{}, err
			}
			return searchDocResult{Results: hits}, nil
		},
	)
}

type semanticSearchResult struct {
	Results []docstore.VectorHit `json:"results"`
}

func newSemanticSearchDocument(d Deps) (tool.Tool, error) {
	if d.Vector == nil {
		return nil, fmt.Errorf("semantic_search_document requires a vector index")
	}
	return functiontool.New[searchDocArgs, semanticSearchResult](
		functiontool.Config{
			Name: "semantic_search_document",
			Description: "Tool to search stored documents by meaning (semantic/vector search over chunks). " +
				"Returns {results: [{doc_id, chunk, score}]}, most similar first; use load_document with a " +
				"doc_id to read the full record. For exact words or tags use search_document instead.",
		},
		func(tc agent.ToolContext, a searchDocArgs) (semanticSearchResult, error) {
			hits, err := d.Vector.Search(tc, strings.TrimSpace(a.Query), a.Size)
			if err != nil {
				return semanticSearchResult{}, err
			}
			return semanticSearchResult{Results: hits}, nil
		},
	)
}

type updateDocArgs struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Content   string   `json:"content"`
	Summary   string   `json:"summary"`
	Tags      []string `json:"tags"`
	Series    string   `json:"series"`
	DateMonth string   `json:"date_month"`
}

func newUpdateDocument(d Deps) (tool.Tool, error) {
	if d.DocStore == nil {
		return nil, fmt.Errorf("update_document requires a document store")
	}
	return functiontool.New[updateDocArgs, docIDResult](
		functiontool.Config{
			Name: "update_document",
			Description: "Tool to correct or clean a stored document by id. Only the fields you provide change; " +
				"omitted fields keep their current value. Returns {id}, or an error if no such id exists.",
		},
		func(tc agent.ToolContext, a updateDocArgs) (docIDResult, error) {
			return updateDoc(tc, d.DocStore, a)
		},
	)
}

func updateDoc(ctx context.Context, store docstore.DocStore, a updateDocArgs) (docIDResult, error) {
	doc, ok, err := store.Get(ctx, strings.TrimSpace(a.ID))
	if err != nil {
		return docIDResult{}, err
	}
	if !ok {
		return docIDResult{}, fmt.Errorf("update_document: no document with id %q", a.ID)
	}
	// Overlay only the provided fields (empty ⇒ keep existing).
	if a.Title != "" {
		doc.Title = a.Title
	}
	if a.Content != "" {
		doc.Content = a.Content
	}
	if a.Summary != "" {
		doc.Summary = a.Summary
	}
	if a.Tags != nil {
		doc.Tags = a.Tags
	}
	if a.Series != "" {
		doc.Series = a.Series
	}
	if a.DateMonth != "" {
		doc.DateMonth = a.DateMonth
	}
	if err := store.Update(ctx, doc); err != nil {
		return docIDResult{}, err
	}
	return docIDResult{ID: doc.ID}, nil
}
