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

	extsdk "github.com/fagerbergj/quack-extensions/sdk"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/schema"
	"github.com/fagerbergj/quack/internal/workspace"
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

// SweepMemories runs the forgetting-rule sweep (epic #1255 P3) on demand
// against every configured store - the same Store.ForgetSweep the nightly
// consolidation job calls, so there is exactly one sweep code path.
func (h *Handler) SweepMemories(w http.ResponseWriter, r *http.Request) {
	var body schema.SweepMemoriesBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		errMsg(w, http.StatusBadRequest, "malformed request body")
		return
	}
	dryRun := body.DryRun != nil && *body.DryRun

	type named struct {
		name string
		st   *memory.Store
	}
	var stores []named
	if h.taskMem != nil {
		stores = append(stores, named{"task", h.taskMem})
	}
	if h.userMem != nil {
		stores = append(stores, named{"user", h.userMem})
	}

	// A later store's failure must not discard an earlier store's already-applied
	// report - sweep is idempotent, so callers can retry the failing store alone.
	out := schema.SweepMemoriesResult{DryRun: dryRun, Stores: []schema.SweepStoreResult{}}
	var sweepErrs []struct {
		Message string `json:"message"`
		Store   string `json:"store"`
	}
	for _, s := range stores {
		report, err := s.st.ForgetSweep(r.Context(), dryRun)
		if err != nil {
			sweepErrs = append(sweepErrs, struct {
				Message string `json:"message"`
				Store   string `json:"store"`
			}{Message: err.Error(), Store: s.name})
			continue
		}
		out.Stores = append(out.Stores, sweepReportWire(s.name, report))
	}
	if len(sweepErrs) > 0 {
		out.Errors = &sweepErrs
	}
	writeJSON(w, http.StatusOK, out)
}

func sweepReportWire(storeName string, report memory.ForgettingReport) schema.SweepStoreResult {
	rules := make([]schema.SweepRuleResult, len(report.Rules))
	for i, r := range report.Rules {
		examples := make([]struct {
			Content string `json:"content"`
			Id      string `json:"id"`
		}, len(r.Examples))
		for j, e := range r.Examples {
			examples[j] = struct {
				Content string `json:"content"`
				Id      string `json:"id"`
			}{Content: e.Content, Id: e.ID}
		}
		rules[i] = schema.SweepRuleResult{
			Index: r.Index, When: r.When, Then: schema.SweepRuleResultThen(r.Then), Matched: r.Matched, Examples: &examples,
		}
	}
	return schema.SweepStoreResult{Store: storeName, Evaluated: report.Evaluated, Kept: report.Kept, Rules: rules}
}

// RescopeMemories moves role:* memories whose provenance chat has a GitHub
// origin into that repo's bucket (#1262). dry run by default; apply:true in
// the body writes the change and audits it.
func (h *Handler) RescopeMemories(w http.ResponseWriter, r *http.Request) {
	var body schema.RescopeMemoriesBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		errMsg(w, http.StatusBadRequest, "malformed request body")
		return
	}
	apply := body.Apply != nil && *body.Apply

	byRepo := map[string]*memory.RescopeRepoStat{}
	skippedNoProvenance := 0
	for _, st := range h.memStores() {
		res, err := st.Rescope(r.Context(), h.chatRepo, apply)
		if err != nil {
			httpError(w, http.StatusInternalServerError, err)
			return
		}
		skippedNoProvenance += res.SkippedNoProvenance
		for repo, stat := range res.ByRepo {
			merged := byRepo[repo]
			if merged == nil {
				merged = &memory.RescopeRepoStat{}
				byRepo[repo] = merged
			}
			merged.Count += stat.Count
			if len(merged.Examples) < 3 {
				merged.Examples = append(merged.Examples, stat.Examples...)
			}
		}
	}

	repos := make([]schema.RescopeRepoTally, 0, len(byRepo))
	for repo, stat := range byRepo {
		examples := stat.Examples
		repos = append(repos, schema.RescopeRepoTally{Repo: repo, Count: stat.Count, Examples: &examples})
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].Repo < repos[j].Repo })
	writeJSON(w, http.StatusOK, schema.RescopeReport{Applied: apply, Repos: repos, SkippedNoProvenance: &skippedNoProvenance})
}

// chatRepo resolves a chat's stored GitHub origin (the "repo" Labels
// dimension the github extension stamps at dispatch) to the same identity
// format workspace.RepoIdentity produces, so a rescoped point lands in the
// SAME bucket a live worker's RepoKey would compute.
func (h *Handler) chatRepo(ctx context.Context, chatID string) (string, bool) {
	c, err := h.store.GetChat(ctx, chatID)
	if err != nil || c == nil || c.Origin == "" {
		return "", false
	}
	var origin extsdk.ChatOrigin
	if err := json.Unmarshal([]byte(c.Origin), &origin); err != nil {
		return "", false
	}
	vals, ok := origin.Labels["repo"]
	if !ok || len(vals) == 0 || vals[0].Value == "" {
		return "", false
	}
	repoKey := workspace.NormalizeRepoURL("github.com/" + vals[0].Value)
	if repoKey == "" {
		return "", false
	}
	return repoKey, true
}

// defaultStatsWeeks is `GET /api/v1/memories/stats`'s weeks default when the
// query param is omitted (epic #1255 P5) - a quarter's worth at a glance.
const defaultStatsWeeks = 12

// GetMemoryStats serves weekly recall precision/support-share/vote/recall
// counts plus a live/invalidated snapshot per scope (epic #1255 P5),
// computed from every chat's ledger (memory.recall/memory.vote entries) and
// memory_ops - no new tables. Weeks with no ledger/memory_ops activity yet
// still appear, zeroed, so the caller can chart a continuous series.
func (h *Handler) GetMemoryStats(w http.ResponseWriter, r *http.Request, params schema.GetMemoryStatsParams) {
	weeks := defaultStatsWeeks
	if params.Weeks != nil && *params.Weeks > 0 {
		weeks = *params.Weeks
	}
	now := time.Now().UTC()

	votes, recalls, err := memoryLedgerEvents(r.Context(), h.ledgerStore, now, weeks)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}

	var ops []memory.OpEvent
	if h.store != nil {
		since := now.AddDate(0, 0, -7*weeks)
		rows, err := h.store.ListMemoryOps(r.Context(), since)
		if err != nil {
			httpError(w, http.StatusInternalServerError, err)
			return
		}
		for _, row := range rows {
			ops = append(ops, memory.OpEvent{Op: row.Op, At: row.Timestamp})
		}
	}

	weekStats := memory.ComputeStats(now, weeks, votes, recalls, ops)

	var scopes []memory.ScopeStats
	for _, st := range h.memStores() {
		s, _, err := st.Snapshot(r.Context())
		if err != nil {
			httpError(w, http.StatusInternalServerError, err)
			return
		}
		scopes = mergeScopeStats(scopes, s)
	}

	writeJSON(w, http.StatusOK, schema.MemoryStats{Weeks: weekStatsWire(weekStats), Scopes: scopeStatsWire(scopes)})
}

// memoryLedgerEvents scans every chat's ledger (h.ledgerStore.List, then one
// ReadEntries per chat) for memory.recall/memory.vote entries within the
// weeks window - the only way to get memory usage across ALL chats, since
// the ledger is per-chat and there is no cross-chat memory index. nil
// ledgerStore (recording disabled) yields no events, not an error.
func memoryLedgerEvents(ctx context.Context, led ledger.LedgerStore, now time.Time, weeks int) ([]memory.VoteEvent, []memory.RecallEvent, error) {
	if led == nil {
		return nil, nil, nil
	}
	since := now.AddDate(0, 0, -7*weeks)
	chats, err := led.List(ctx)
	if err != nil {
		return nil, nil, err
	}
	var votes []memory.VoteEvent
	var recalls []memory.RecallEvent
	for _, c := range chats {
		entries, err := led.ReadEntries(ctx, c.ID, 0)
		if err != nil {
			return nil, nil, err
		}
		for _, e := range entries {
			if e.At.Before(since) {
				continue
			}
			switch e.Kind {
			case ledger.KindMemoryVote:
				var p ledger.MemoryVotePayload
				if err := json.Unmarshal(e.Payload, &p); err != nil {
					continue
				}
				votes = append(votes, memory.VoteEvent{MemoryID: p.MemoryID, Vote: string(p.Vote), At: e.At})
			case ledger.KindMemoryRecall:
				var p ledger.MemoryRecallPayload
				if err := json.Unmarshal(e.Payload, &p); err != nil {
					continue
				}
				for range p.Entries {
					recalls = append(recalls, memory.RecallEvent{At: e.At})
				}
			}
		}
	}
	return votes, recalls, nil
}

// mergeScopeStats sums two stores' per-scope tallies (task + user memory can
// both use a "user:" or "role:" bucket only in principle, but never the same
// key in practice - summed defensively either way).
func mergeScopeStats(acc []memory.ScopeStats, add []memory.ScopeStats) []memory.ScopeStats {
	byScope := make(map[string]memory.ScopeStats, len(acc)+len(add))
	for _, s := range acc {
		byScope[s.Scope] = s
	}
	for _, s := range add {
		cur := byScope[s.Scope]
		cur.Scope = s.Scope
		cur.Live += s.Live
		cur.Invalidated += s.Invalidated
		byScope[s.Scope] = cur
	}
	out := make([]memory.ScopeStats, 0, len(byScope))
	for _, s := range byScope {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Scope < out[j].Scope })
	return out
}

func weekStatsWire(weeks []memory.WeekStats) []schema.MemoryWeekStats {
	out := make([]schema.MemoryWeekStats, len(weeks))
	for i, w := range weeks {
		out[i] = schema.MemoryWeekStats{
			Week: w.Week, Recalls: w.Recalls, Supported: w.Supported, Contradicted: w.Contradicted,
			NotRelevant: w.NotRelevant, Precision: w.Precision, SupportShare: w.SupportShare,
			Minted: w.Minted, Invalidated: w.Invalidated,
		}
	}
	return out
}

func scopeStatsWire(scopes []memory.ScopeStats) []schema.MemoryScopeStats {
	out := make([]schema.MemoryScopeStats, len(scopes))
	for i, s := range scopes {
		out[i] = schema.MemoryScopeStats{Scope: s.Scope, Live: s.Live, Invalidated: s.Invalidated}
	}
	return out
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
			status = schema.MemoryStatusUnverified
		}
		w.Status = &status
		count := m.ReinforcementCount
		w.ReinforcementCount = &count
		if m.InvalidationReason != "" {
			reason := m.InvalidationReason
			w.InvalidationReason = &reason
		}
		upvotes, downvotes, voteScore := m.Upvotes, m.Downvotes, m.VoteScore
		w.Upvotes, w.Downvotes, w.VoteScore = &upvotes, &downvotes, &voteScore
		tier := schema.MemoryTier(m.Tier)
		if tier == "" {
			tier = schema.MemoryTierUnverified
		}
		w.Tier = &tier
		recalls := m.Recalls
		w.Recalls = &recalls
		if t, err := time.Parse(time.RFC3339, m.LastUpvotedAt); err == nil {
			w.LastUpvotedAt = &t
		}
		if t, err := time.Parse(time.RFC3339, m.LastRecalledAt); err == nil {
			w.LastRecalledAt = &t
		}
		if len(m.AbsorbedIDs) > 0 {
			ids := m.AbsorbedIDs
			w.AbsorbedIds = &ids
		}
		if ov := ownVoteWire(m.HumanVote); ov != nil {
			w.OwnVote = ov
		}
		out[i] = w
	}
	return out
}

// ownVoteWire maps the store's internal "up"/"down"/"" to the wire enum -
// "" (never voted, or last voted "none") comes back as a nil pointer, so
// the field is simply absent from the JSON.
func ownVoteWire(humanVote string) *schema.MemoryOwnVote {
	switch humanVote {
	case memory.HumanVoteUp:
		v := schema.MemoryOwnVoteUp
		return &v
	case memory.HumanVoteDown:
		v := schema.MemoryOwnVoteDown
		return &v
	default:
		return nil
	}
}

// VoteMemory casts or clears the human's own vote on one memory (epic #1255
// P4). Fail-closed: the memory.vote ledger entry is appended before the
// point is mutated, under the memory's provenance chat if it has one, else
// a fixed "human" chat key (chat-less human votes need a ledger home, and
// nothing else keys entries by anything BUT a chat).
func (h *Handler) VoteMemory(w http.ResponseWriter, r *http.Request, memoryID schema.MemoryID) {
	var body schema.VoteMemoryBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errMsg(w, http.StatusBadRequest, "malformed request body")
		return
	}
	vote := strings.ToLower(strings.TrimSpace(string(body.Vote)))
	switch vote {
	case memory.HumanVoteUp, memory.HumanVoteDown, memory.HumanVoteNone:
	default:
		errMsg(w, http.StatusBadRequest, "vote must be up, down, or none")
		return
	}
	reason := ""
	if body.Reason != nil {
		reason = *body.Reason
	}

	st, m, err := findMemoryByID(r.Context(), h.memStores(), memoryID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	if st == nil {
		errMsg(w, http.StatusNotFound, "not found")
		return
	}

	if h.ledgerStore != nil {
		chatID := m.ChatID
		if chatID == "" {
			chatID = humanVoteChatKey
		}
		payload, err := json.Marshal(ledger.MemoryVotePayload{MemoryID: memoryID, Vote: humanLedgerVote(vote), Reason: reason, Actor: string(memory.ActorHuman)})
		if err != nil {
			httpError(w, http.StatusInternalServerError, err)
			return
		}
		if _, err := h.ledgerStore.AppendIntent(r.Context(), ledger.Entry{
			ChatID: chatID, Kind: ledger.KindMemoryVote, At: time.Now().UTC(), Payload: payload,
		}); err != nil {
			httpError(w, http.StatusInternalServerError, err)
			return
		}
	}

	if err := st.SetHumanVote(r.Context(), memoryID, vote); err != nil {
		if errors.Is(err, memory.ErrMemoryNotFound) {
			errMsg(w, http.StatusNotFound, "not found")
			return
		}
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	_, updated, err := findMemoryByID(r.Context(), []*memory.Store{st}, memoryID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, memoriesWire([]memory.Memory{updated})[0])
}

// humanVoteChatKey is the fixed ledger chat id for a human vote on a memory
// with no provenance chat (minted before #875, or never associated with a
// chat) - the ledger has no chat-less entry shape, so this is its home.
const humanVoteChatKey = "human"

// humanLedgerVote maps up/down/none to the judge's supported/contradicted
// vocabulary so a human vote uses the same ledger vote kind; "none" (a
// retraction, not itself a stance) logs as not_relevant, still an audit
// trail of what happened even though it carries no score delta.
func humanLedgerVote(vote string) ledger.MemoryVote {
	switch vote {
	case memory.HumanVoteUp:
		return ledger.MemoryVoteSupported
	case memory.HumanVoteDown:
		return ledger.MemoryVoteContradicted
	default:
		return ledger.MemoryVoteNotRelevant
	}
}

// findMemoryByID scans the given stores (list+scan, same cost profile as
// the rest of this file at memory's documented hundreds-thousands scale;
// there is no store.GetByID) for id, including invalidated points so a vote
// target is still found mid-race. Returns a nil store, zero Memory if none
// of the stores has it.
func findMemoryByID(ctx context.Context, stores []*memory.Store, id string) (*memory.Store, memory.Memory, error) {
	for _, st := range stores {
		mems, _, err := st.List(ctx, nil, 0, 0, true)
		if err != nil {
			return nil, memory.Memory{}, err
		}
		for _, m := range mems {
			if m.ID == id {
				return st, m, nil
			}
		}
	}
	return nil, memory.Memory{}, nil
}

// ListNodeMemories folds one node's memory.recall/memory.vote ledger entries
// into its received-memories set (epic #1255 P4), enriched with each
// memory's current content/tier from the store (the ledger entries carry
// only id+score, not the point's live fields) and the human's own vote.
func (h *Handler) ListNodeMemories(w http.ResponseWriter, r *http.Request, chatID schema.ChatID, nodeID schema.NodeID) {
	out := schema.NodeMemoryList{Memories: []schema.NodeMemory{}}
	if h.ledgerStore == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	entries, err := h.ledgerStore.ReadEntries(r.Context(), chatID, 0)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}

	type recallInfo struct {
		source string
		score  float32
	}
	recalls := map[string]recallInfo{}
	var order []string
	type voteInfo struct {
		vote, reason string
	}
	votes := map[string]voteInfo{}

	for _, e := range entries {
		if e.NodeID != nodeID {
			continue
		}
		switch e.Kind {
		case ledger.KindMemoryRecall:
			var p ledger.MemoryRecallPayload
			if json.Unmarshal(e.Payload, &p) != nil {
				continue
			}
			for _, m := range p.Entries {
				if _, ok := recalls[m.ID]; !ok {
					order = append(order, m.ID)
				}
				recalls[m.ID] = recallInfo{source: p.Source, score: m.Score}
			}
		case ledger.KindMemoryVote:
			var p ledger.MemoryVotePayload
			if json.Unmarshal(e.Payload, &p) != nil {
				continue
			}
			votes[p.MemoryID] = voteInfo{vote: string(p.Vote), reason: p.Reason}
		}
	}
	if len(order) == 0 {
		writeJSON(w, http.StatusOK, out)
		return
	}

	stores := h.memStores()
	content := map[string]memory.Memory{}
	for _, st := range stores {
		mems, _, err := st.List(r.Context(), nil, 0, 0, true)
		if err != nil {
			httpError(w, http.StatusInternalServerError, err)
			return
		}
		for _, m := range mems {
			content[m.ID] = m
		}
	}

	for _, id := range order {
		ri := recalls[id]
		nm := schema.NodeMemory{Id: id, Source: schema.NodeMemorySource(ri.source)}
		score := ri.score
		nm.Score = &score
		if m, ok := content[id]; ok {
			c := m.Content
			nm.Content = &c
			tier := schema.NodeMemoryTier(m.Tier)
			if tier == "" {
				tier = schema.Unverified
			}
			nm.Tier = &tier
			if ov := m.HumanVote; ov == memory.HumanVoteUp || ov == memory.HumanVoteDown {
				own := schema.NodeMemoryOwnVote(ov)
				nm.OwnVote = &own
			}
		}
		if v, ok := votes[id]; ok {
			vote := schema.NodeMemoryVote(v.vote)
			nm.Vote = &vote
			if v.reason != "" {
				r := v.reason
				nm.Reason = &r
			}
		}
		out.Memories = append(out.Memories, nm)
	}
	writeJSON(w, http.StatusOK, out)
}
