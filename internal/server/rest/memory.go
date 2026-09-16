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

// listMemoriesArgs is the decoded state of one list/search query.
type listMemoriesArgs struct {
	bucketFilter       string
	buckets            []string
	limit              int
	sortBy             string
	offset             int
	includeInvalidated bool
	tier               string
}

// listMemoriesBase decodes the bucket filter and limit - everything the q
// (search) branch needs before paging params are validated at all.
func listMemoriesBase(params schema.ListMemoriesParams) *listMemoriesArgs {
	a := &listMemoriesArgs{}
	if params.Bucket != nil && strings.TrimSpace(*params.Bucket) != "" {
		a.bucketFilter = strings.TrimSpace(*params.Bucket)
		a.buckets = []string{a.bucketFilter}
	}
	a.limit = memory.DefaultListLimit
	if params.Limit != nil && *params.Limit > 0 {
		a.limit = *params.Limit
	}
	if a.limit > memory.MemoryPageMaxLimit {
		a.limit = memory.MemoryPageMaxLimit
	}
	return a
}

// errListMemoriesSort is a bad `sort` value; the handler answers 400 with its text.
var errListMemoriesSort = errors.New("sort must be one of the documented values")

// listMemoriesPaging decodes sort, page token, includeInvalidated, and tier.
// A non-nil error is a 400: errListMemoriesSort or a bad page token.
func (a *listMemoriesArgs) listMemoriesPaging(params schema.ListMemoriesParams) error {
	a.sortBy = memory.SortNewest
	if params.Sort != nil {
		if !params.Sort.Valid() {
			return errListMemoriesSort
		}
		a.sortBy = string(*params.Sort)
	}
	if params.PageToken != nil && *params.PageToken != "" {
		// Bound to sortBy too (not just bucketFilter): an offset from one sort
		// order names a different row under another, so a page_token replayed
		// against a changed `sort` must 400, not silently return the wrong page.
		off, err := memory.DecodePageToken(*params.PageToken, a.bucketFilter, a.sortBy)
		if err != nil {
			return err
		}
		a.offset = off
	}
	if params.IncludeInvalidated != nil {
		a.includeInvalidated = *params.IncludeInvalidated
	}
	if params.Tier != nil {
		a.tier = string(*params.Tier)
	}
	return nil
}

// listMemoriesPage runs the store list call and adds the next-page token
// while a page remains.
func (a *listMemoriesArgs) listMemoriesPage(ctx context.Context, stores []*memory.Store) (schema.MemoryList, error) {
	mems, total, err := listMemories(ctx, stores, a.buckets, a.offset, a.limit, a.includeInvalidated, a.tier, a.sortBy)
	if err != nil {
		return schema.MemoryList{}, err
	}
	out := schema.MemoryList{Memories: memoriesWire(mems), Total: total}
	if next := a.offset + len(mems); len(mems) > 0 && next < total {
		tok := memory.EncodePageToken(a.bucketFilter, a.sortBy, next)
		out.NextPageToken = &tok
	}
	return out, nil
}

// ListMemories browses (or, with `q`, searches) every configured memory store.
// A bucket filter is passed to each store as-is - a store that doesn't own that
// bucket just contributes nothing, so no prefix-routing guesswork is needed.
func (h *Handler) ListMemories(w http.ResponseWriter, r *http.Request, params schema.ListMemoriesParams) {
	stores := h.memStores()
	a := listMemoriesBase(params)

	if params.Q != nil && strings.TrimSpace(*params.Q) != "" {
		mems, err := searchMemories(r.Context(), stores, a.buckets, *params.Q, a.limit)
		if err != nil {
			httpError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, schema.MemoryList{Memories: memoriesWire(mems), Total: len(mems)})
		return
	}

	if err := a.listMemoriesPaging(params); err != nil {
		// errListMemoriesSort and a bad page token are both 400; the former carries its own text.
		if errors.Is(err, errListMemoriesSort) {
			errMsg(w, http.StatusBadRequest, err.Error())
			return
		}
		httpError(w, http.StatusBadRequest, err)
		return
	}

	out, err := a.listMemoriesPage(r.Context(), stores)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// GetMemory is a direct per-id lookup (findMemoryByID), not a List scan.
func (h *Handler) GetMemory(w http.ResponseWriter, r *http.Request, memoryID schema.MemoryID) {
	_, m, err := findMemoryByID(r.Context(), h.memStores(), memoryID)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	if m.ID == "" {
		errMsg(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, memoriesWire([]memory.Memory{m})[0])
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
// only one; with two (task + user both enabled) it fetches each store's
// matching set (capped at DefaultListLimit=50 per store - see the ponytail comment below), merges, and pages in Go, which is the only way to keep offset/limit meaningful across two independent backends. includeInvalidated rides straight through to the index-level filter (Store.List) so the total and the page both agree, rather than a Go post-filter after paging.
func listMemories(ctx context.Context, stores []*memory.Store, buckets []string, offset, limit int, includeInvalidated bool, tier, sortBy string) ([]memory.Memory, int, error) {
	if len(stores) == 0 {
		return nil, 0, nil
	}
	if len(stores) == 1 {
		return stores[0].List(ctx, buckets, offset, limit, includeInvalidated, tier, sortBy)
	}
	var all []memory.Memory
	for _, st := range stores {
		// ponytail: 0,0 does NOT fetch each store's full matching set - Store.List
		// coerces limit<=0 to DefaultListLimit (50, store.go), so two-store mode
		// is only correct up to ~50 matches per store (~100 combined); beyond that, total/every page/next_page_token silently go wrong. Predates this PR (not introduced by sortBy) - fix is an unbounded List variant or an explicit high cap, if a deployment's corpus ever needs it.
		mems, _, err := st.List(ctx, buckets, 0, 0, includeInvalidated, tier, sortBy)
		if err != nil {
			return nil, 0, err
		}
		all = append(all, mems...)
	}
	memory.SortMemories(all, sortBy)
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

// sweepStoreErr aliases the report's per-store error shape so out.Errors stays assignable.
type sweepStoreErr = struct {
	Message string `json:"message"`
	Store   string `json:"store"`
}

// namedSweepStore is a memory backend plus the name it reports under.
type namedSweepStore struct {
	name string
	st   *memory.Store
}

// sweepStores lists every configured memory backend in report order.
func (h *Handler) sweepStores() []namedSweepStore {
	var stores []namedSweepStore
	if h.taskMem != nil {
		stores = append(stores, namedSweepStore{"task", h.taskMem})
	}
	if h.userMem != nil {
		stores = append(stores, namedSweepStore{"user", h.userMem})
	}
	return stores
}

// runForgetSweep runs ForgetSweep over every store; a store's failure lands in
// errs and never discards an earlier store's already-applied report.
func runForgetSweep(ctx context.Context, stores []namedSweepStore, dryRun bool) ([]schema.SweepStoreResult, []sweepStoreErr) {
	out := []schema.SweepStoreResult{}
	var errs []sweepStoreErr
	for _, s := range stores {
		report, err := s.st.ForgetSweep(ctx, dryRun)
		if err != nil {
			errs = append(errs, sweepStoreErr{Message: err.Error(), Store: s.name})
			continue
		}
		out = append(out, sweepReportWire(s.name, report))
	}
	return out, errs
}

// runDedupeSweep runs DedupeSweep over every store with the same error handling;
// results stays nil when no store succeeded, so the caller omits the field.
func runDedupeSweep(ctx context.Context, stores []namedSweepStore, apply bool) ([]schema.SweepDedupeStoreResult, []sweepStoreErr) {
	var results []schema.SweepDedupeStoreResult
	var errs []sweepStoreErr
	for _, s := range stores {
		report, err := s.st.DedupeSweep(ctx, apply)
		if err != nil {
			errs = append(errs, sweepStoreErr{Message: err.Error(), Store: s.name})
			continue
		}
		results = append(results, dedupeReportWire(s.name, report))
	}
	return results, errs
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
	dedupe := body.Dedupe != nil && *body.Dedupe
	apply := body.Apply != nil && *body.Apply

	// A later store's failure must not discard an earlier store's already-applied
	// report - sweep is idempotent, so callers can retry the failing store alone.
	reportedDryRun := dryRun
	if dedupe {
		reportedDryRun = !apply
	}
	out := schema.SweepMemoriesResult{DryRun: reportedDryRun, Stores: []schema.SweepStoreResult{}}
	var sweepErrs []sweepStoreErr
	if dedupe {
		var results []schema.SweepDedupeStoreResult
		results, sweepErrs = runDedupeSweep(r.Context(), h.sweepStores(), apply)
		if results != nil {
			out.Dedupe = &results
		}
	} else {
		var stores []schema.SweepStoreResult
		stores, sweepErrs = runForgetSweep(r.Context(), h.sweepStores(), dryRun)
		out.Stores = stores
	}
	if len(sweepErrs) > 0 {
		out.Errors = &sweepErrs
	}
	writeJSON(w, http.StatusOK, out)
}

func dedupeReportWire(storeName string, report memory.DedupeReport) schema.SweepDedupeStoreResult {
	clusters := make([]schema.SweepDedupeCluster, len(report.Clusters))
	for i, c := range report.Clusters {
		examples := make([]schema.SweepDedupeExample, len(c.Examples))
		for j, e := range c.Examples {
			examples[j] = schema.SweepDedupeExample{Id: e.ID, Content: e.Content}
		}
		clusters[i] = schema.SweepDedupeCluster{Bucket: c.Bucket, Size: c.Size, Examples: examples}
	}
	return schema.SweepDedupeStoreResult{
		Store: storeName, Applied: report.Applied, NumClusters: report.NumClusters,
		LlmCalls: report.LLMCalls, OpsApplied: report.OpsApplied, Dropped: report.Dropped,
		Clusters: clusters,
	}
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
// format workspace.RepoIdentity produces, so a rescoped point lands in the SAME bucket a live worker's RepoKey would compute.
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

// GetMemoryStats serves weekly recall precision/vote/recall counts plus a per-scope
// live/invalidated/never-recalled/no-votes/unsupported-verified snapshot,
// computed from every chat's ledger (memory.recall/memory.vote entries) and memory_ops - no new tables. Weeks with no ledger/memory_ops activity yet still appear, zeroed, so the caller can chart a continuous series.
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

// memoryLedgerEvents scans every chat's ledger for memory.recall/memory.vote entries
// within the weeks window - the only way to get memory usage across ALL chats, since the
// ledger is per-chat and there is no cross-chat memory index. Pushes both the kind and `at >= since` filters into one query via ReadAllByKindsSince instead of List() plus one ReadByKinds per chat plus a Go-side window filter (perf audit #12: 94 queries and 0.8-5.0s per memory-page load on a 465k-entry ledger). nil ledgerStore (recording disabled) yields no events, not an error.
func memoryLedgerEvents(ctx context.Context, led ledger.LedgerStore, now time.Time, weeks int) ([]memory.VoteEvent, []memory.RecallEvent, error) {
	if led == nil {
		return nil, nil, nil
	}
	since := now.AddDate(0, 0, -7*weeks)
	kinds := []string{ledger.KindMemoryVote, ledger.KindMemoryRecall}
	entries, err := ledger.ReadAllByKindsSince(ctx, led, kinds, since)
	if err != nil {
		return nil, nil, err
	}
	var votes []memory.VoteEvent
	var recalls []memory.RecallEvent
	for _, e := range entries {
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
		cur.NeverRecalled += s.NeverRecalled
		cur.NoVotes += s.NoVotes
		cur.UnsupportedVerified += s.UnsupportedVerified
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
			NotRelevant: w.NotRelevant, Precision: w.Precision,
			Minted: w.Minted, Invalidated: w.Invalidated,
		}
	}
	return out
}

func scopeStatsWire(scopes []memory.ScopeStats) []schema.MemoryScopeStats {
	out := make([]schema.MemoryScopeStats, len(scopes))
	for i, s := range scopes {
		out[i] = schema.MemoryScopeStats{
			Scope: s.Scope, Live: s.Live, Invalidated: s.Invalidated,
			NeverRecalled: s.NeverRecalled, NoVotes: s.NoVotes, UnsupportedVerified: s.UnsupportedVerified,
		}
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
		supported, notRelevant := m.Supported, m.NotRelevant
		w.Supported, w.NotRelevant = &supported, &notRelevant
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
// point is mutated, under the memory's provenance chat if it has one, else a fixed "human" chat key (chat-less human votes need a ledger home, and nothing else keys entries by anything BUT a chat).
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
	// Validated BEFORE the ledger entry is appended (#1265 review finding 3):
	// an invalidated memory gets neither an orphan memory.vote entry nor a
	// misleading "not found" - a clear 409 instead.
	if m.Status == string(memory.StatusInvalidated) {
		errMsg(w, http.StatusConflict, "memory is invalidated")
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
// vocabulary so a human vote uses the same ledger vote kind. "none" (a
// retraction, not itself a stance) logs as not_relevant, still an audit trail of what happened even though it carries no score delta.
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

// findMemoryByID tries each store's direct GetByID in turn (correct
// regardless of how many memories exist or which List page id would land
// on - #1265 review finding 1) and returns the first hit, including an invalidated point so the caller can decide what that means for its purpose. Returns a nil store, zero Memory if none of the stores has it.
func findMemoryByID(ctx context.Context, stores []*memory.Store, id string) (*memory.Store, memory.Memory, error) {
	for _, st := range stores {
		m, err := st.GetByID(ctx, id)
		if err == nil {
			return st, m, nil
		}
		if !errors.Is(err, memory.ErrMemoryNotFound) {
			return nil, memory.Memory{}, err
		}
	}
	return nil, memory.Memory{}, nil
}

// ListNodeMemories folds one node's memory.recall/memory.vote ledger entries
// into its received-memories set (epic #1255 P4), enriched with each
// memory's current content/tier from the store (the ledger entries carry only id+score, not the point's live fields) and the human's own vote.
func (h *Handler) ListNodeMemories(w http.ResponseWriter, r *http.Request, chatID schema.ChatID, nodeID schema.NodeID) {
	if !h.requireChat(w, r, chatID) {
		return
	}
	out := schema.NodeMemoryList{Memories: []schema.NodeMemory{}}
	if h.ledgerStore == nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	entries, err := ledger.ReadByKinds(r.Context(), h.ledgerStore, chatID, 0, []string{ledger.KindMemoryVote, ledger.KindMemoryRecall})
	if err != nil {
		httpError(w, http.StatusInternalServerError, err)
		return
	}
	order, recalls, votes := nodeMemorySignals(entries, nodeID)
	if len(order) == 0 {
		writeJSON(w, http.StatusOK, out)
		return
	}
	// Direct per-id lookup (#1265 review finding 1), not a bulk List+scan - correct
	// regardless of corpus size, and include-invalidated by design.
	content, ok := h.nodeMemoryContent(w, r, order)
	if !ok {
		return
	}
	out.Memories = nodeMemoryWire(order, recalls, votes, content)
	writeJSON(w, http.StatusOK, out)
}

type recallInfo struct {
	source string
	score  float32
}

type voteInfo struct {
	vote, reason string
}

// nodeMemorySignals: fold this node's memory recall/vote entries into ordered ids plus
// the per-id recall/vote signals.
func nodeMemorySignals(entries []ledger.Entry, nodeID schema.NodeID) ([]string, map[string]recallInfo, map[string]voteInfo) {
	recalls := map[string]recallInfo{}
	var order []string
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
	return order, recalls, votes
}

// nodeMemoryContent: direct per-id lookup (#1265 review finding 1); ok=false after an
// httpError when a lookup fails hard.
func (h *Handler) nodeMemoryContent(w http.ResponseWriter, r *http.Request, order []string) (map[string]memory.Memory, bool) {
	stores := h.memStores()
	content := map[string]memory.Memory{}
	for _, id := range order {
		if _, m, err := findMemoryByID(r.Context(), stores, id); err == nil && m.ID != "" {
			content[id] = m
		} else if err != nil {
			httpError(w, http.StatusInternalServerError, err)
			return content, false
		}
	}
	return content, true
}

// nodeMemoryWire: shape one NodeMemory per recalled id (content/tier/own vote when the
// memory still resolves, plus the latest vote/reason).
func nodeMemoryWire(order []string, recalls map[string]recallInfo, votes map[string]voteInfo, content map[string]memory.Memory) []schema.NodeMemory {
	var out []schema.NodeMemory
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
				tier = schema.NodeMemoryTierUnverified
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
		out = append(out, nm)
	}
	return out
}
