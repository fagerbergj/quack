package rest

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/schema"
)

// memStores is every configured memory backend, task (repo:/role:) and user
// (user:), in the order results are merged. Both may be disabled (nil filtered out).
func (h *Handler) memStores() []*memory.Store {
	stores := make([]*memory.Store, 0, 2)
	if h.taskMem != nil {
		stores = append(stores, h.taskMem)
	}
	if h.userMem != nil {
		stores = append(stores, h.userMem)
	}
	return stores
}

// ListMemories browses (or, with `q`, searches) every configured memory store.
// A bucket filter is passed to each store as-is - a store that doesn't own that
// bucket just contributes nothing, so no prefix-routing guesswork is needed.
func (h *Handler) ListMemories(w http.ResponseWriter, r *http.Request, params schema.ListMemoriesParams) {
	stores := h.memStores()
	bucketFilter := ""
	var buckets []string
	if params.Bucket != nil && strings.TrimSpace(*params.Bucket) != "" {
		bucketFilter = strings.TrimSpace(*params.Bucket)
		buckets = []string{bucketFilter}
	}
	limit := memory.DefaultListLimit
	if params.Limit != nil && *params.Limit > 0 {
		limit = *params.Limit
	}
	if limit > memory.MemoryPageMaxLimit {
		limit = memory.MemoryPageMaxLimit
	}

	if params.Q != nil && strings.TrimSpace(*params.Q) != "" {
		mems, err := searchMemories(r.Context(), stores, buckets, *params.Q, limit)
		if err != nil {
			httpError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, schema.MemoryList{Memories: memoriesWire(mems), Total: len(mems)})
		return
	}

	offset := 0
	if params.PageToken != nil && *params.PageToken != "" {
		off, err := memory.DecodePageToken(*params.PageToken, bucketFilter)
		if err != nil {
			httpError(w, http.StatusBadRequest, err)
			return
		}
		offset = off
	}
	includeInvalidated := params.IncludeInvalidated != nil && *params.IncludeInvalidated
	mems, total, err := listMemories(r.Context(), stores, buckets, offset, limit, includeInvalidated)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	out := schema.MemoryList{Memories: memoriesWire(mems), Total: total}
	if next := offset + len(mems); len(mems) > 0 && next < total {
		tok := memory.EncodePageToken(bucketFilter, next)
		out.NextPageToken = &tok
	}
	writeJSON(w, http.StatusOK, out)
}

// DeleteMemory invalidates one memory (soft-delete, design doc §4(b)) - the
// point stays in the index with status=invalidated; nothing is removed. 404
// if no configured store has that id.
func (h *Handler) DeleteMemory(w http.ResponseWriter, r *http.Request, memoryID schema.MemoryID) {
	var body schema.DeleteMemoryBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		errMsg(w, http.StatusBadRequest, "malformed request body")
		return
	}
	reason := "manual delete"
	if body.Reason != nil && strings.TrimSpace(*body.Reason) != "" {
		reason = strings.TrimSpace(*body.Reason)
	}
	err := invalidateMemory(r.Context(), h.memStores(), memoryID, reason)
	if errors.Is(err, memory.ErrMemoryNotFound) {
		errMsg(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listMemories delegates straight to the one configured store when there's
// only one; with two (task + user both enabled) it fetches everything each
// store holds for the filter, merges, and pages in Go - memory's documented
// scale (hundreds-thousands) makes that cheap, and it's the only way to keep
// offset/limit meaningful across two independent backends. includeInvalidated
// rides straight through to the index-level filter (Store.List) so the total
// and the page both agree, rather than a Go post-filter after paging.
func listMemories(ctx context.Context, stores []*memory.Store, buckets []string, offset, limit int, includeInvalidated bool) ([]memory.Memory, int, error) {
	if len(stores) == 0 {
		return nil, 0, nil
	}
	if len(stores) == 1 {
		return stores[0].List(ctx, buckets, offset, limit, includeInvalidated)
	}
	var all []memory.Memory
	for _, st := range stores {
		mems, _, err := st.List(ctx, buckets, 0, 0, includeInvalidated)
		if err != nil {
			return nil, 0, err
		}
		all = append(all, mems...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Timestamp > all[j].Timestamp })
	total := len(all)
	if offset >= total {
		return []memory.Memory{}, total, nil
	}
	end := total
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return all[offset:end], total, nil
}

// searchMemories queries every configured store and merges by score, descending.
func searchMemories(ctx context.Context, stores []*memory.Store, buckets []string, q string, limit int) ([]memory.Memory, error) {
	var all []memory.Memory
	for _, st := range stores {
		mems, err := st.Search(ctx, buckets, q, limit)
		if err != nil {
			return nil, err
		}
		all = append(all, mems...)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Score > all[j].Score })
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

// invalidateMemory tries each configured store in turn; ErrMemoryNotFound
// only if none of them had the id.
func invalidateMemory(ctx context.Context, stores []*memory.Store, id, reason string) error {
	for _, st := range stores {
		err := st.InvalidateByID(ctx, id, reason, memory.ActorHuman)
		if err == nil {
			return nil
		}
		if !errors.Is(err, memory.ErrMemoryNotFound) {
			return err
		}
	}
	return memory.ErrMemoryNotFound
}

func memoriesWire(mems []memory.Memory) []schema.Memory {
	out := make([]schema.Memory, len(mems))
	for i, m := range mems {
		w := schema.Memory{
			Id:      m.ID,
			Content: m.Content,
			Bucket:  m.Bucket,
			Author:  m.Author,
			Kind:    m.Kind,
		}
		if t, err := time.Parse(time.RFC3339, m.Timestamp); err == nil {
			w.Timestamp = t
		}
		if m.Score != 0 {
			score := m.Score
			w.Score = &score
		}
		// Status "" predates the lifecycle fields (design doc §3) and reads as unverified.
		status := schema.MemoryStatus(m.Status)
		if status == "" {
			status = schema.Unverified
		}
		w.Status = &status
		count := m.ReinforcementCount
		w.ReinforcementCount = &count
		if m.InvalidationReason != "" {
			reason := m.InvalidationReason
			w.InvalidationReason = &reason
		}
		out[i] = w
	}
	return out
}
