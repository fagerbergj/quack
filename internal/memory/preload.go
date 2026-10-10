package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/ledger"
)

// preloadInstructions matches ADK's preloadmemorytool wording so recalled memory
// reads the same to the model.
const preloadInstructions = `The following content is from your previous conversations with the user.
They may be useful for answering the user's current query.
<PAST_CONVERSATIONS>
%s
</PAST_CONVERSATIONS>`

// oncePreload replaces ADK's preloadmemorytool to recall once per invocation: ADK runs ProcessRequest on
// every tool-loop step, re-searching a constant query and risking context rot from low-relevance hits.
type oncePreload struct{}

// NewPreload returns a once-per-invocation preload-memory processor.
func NewPreload() tool.Tool { return oncePreload{} }

func (oncePreload) Name() string { return "preload_memory" }
func (oncePreload) Description() string {
	return "Preloads relevant memory once at the start of the turn."
}
func (oncePreload) IsLongRunning() bool { return false }

func (oncePreload) ProcessRequest(ctx adkagent.Context, req *model.LLMRequest) error {
	uc := ctx.UserContent()
	if uc == nil || len(uc.Parts) == 0 || uc.Parts[0] == nil || uc.Parts[0].Text == "" {
		return nil
	}
	query := uc.Parts[0].Text

	// The session is long-lived, so anchor on the latest real user message and skip once the model has
	// produced output after it: recall runs once per turn.
	fs := firstStep(req.Contents)
	slog.Default().Debug("preload", "component", "memory", "contents", len(req.Contents), "first_step", fs)
	if !fs {
		return nil
	}

	resp, err := ctx.SearchMemory(ctx, query)
	if err != nil {
		return fmt.Errorf("preload memory search failed: %w", err)
	}
	if resp == nil || len(resp.Memories) == 0 {
		return nil
	}
	text := formatMemories(resp.Memories)
	if text == "" {
		return nil
	}
	appendInstruction(req, fmt.Sprintf(preloadInstructions, text))
	return nil
}

// firstStep reports whether nothing follows the latest text-bearing user message (function responses are
// textless user content). No user message counts as a first step so recall still runs.
func firstStep(contents []*genai.Content) bool {
	last := -1
	for i, c := range contents {
		if c != nil && c.Role == genai.RoleUser && hasText(c) {
			last = i
		}
	}
	if last < 0 {
		return true
	}
	return last == len(contents)-1
}

func hasText(c *genai.Content) bool {
	for _, p := range c.Parts {
		if p != nil && p.Text != "" {
			return true
		}
	}
	return false
}

func formatMemories(memories []adkmemory.Entry) string {
	var lines []string
	for _, m := range memories {
		t := extractText(m)
		if t == "" {
			continue
		}
		if !m.Timestamp.IsZero() {
			lines = append(lines, "Time: "+m.Timestamp.Format(time.RFC3339))
		}
		if m.Author != "" {
			t = m.Author + ": " + t
		}
		lines = append(lines, t)
	}
	return strings.Join(lines, "\n")
}

func extractText(m adkmemory.Entry) string {
	if m.Content == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range m.Content.Parts {
		if p == nil || p.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(p.Text)
	}
	return b.String()
}

// appendInstruction replicates ADK's internal utils.AppendInstructions (which we
// can't import): append the recalled block to the request's system instruction.
func appendInstruction(r *model.LLMRequest, inst string) {
	if r.Config == nil {
		r.Config = &genai.GenerateContentConfig{}
	}
	si := r.Config.SystemInstruction
	if si == nil {
		r.Config.SystemInstruction = genai.NewContentFromText(inst, genai.RoleUser)
		return
	}
	if n := len(si.Parts); n > 0 && si.Parts[n-1].Text != "" {
		si.Parts[n-1].Text += "\n\n" + inst
		return
	}
	si.Parts = append(si.Parts, genai.NewPartFromText(inst))
}

// recallInstructions frames gate-side recall for an external (ACP) worker, as preloadInstructions
// does for a native agent.
const recallInstructions = `The following notes were remembered from previous runs on this repository /
task family. Use them instead of re-deriving what they already answer; they may
be stale, so verify anything load-bearing against the code itself.
<MEMORY>
%s
</MEMORY>`

// Recall is the gate-side preload_memory for external workers. "" when the store is nil, nothing
// matches, or the embedder is down: recall is best-effort and bounded, never failing a node.
func (s *Store) Recall(ctx context.Context, sc Scope, query string) string {
	text, _ := s.RecallWithHits(ctx, sc, query)
	return text
}

// Delivered is one memory handed to a worker: what the judge votes on and memory.recall records.
// The JSON tags make it recall_memory's tool output too.
type Delivered struct {
	ID      string  `json:"id"`
	Tier    string  `json:"tier"`
	Score   float32 `json:"score"`
	Content string  `json:"content"`
}

// RecallWithHits is Recall plus the delivered set, for ledger logging and judge voting.
// hits is nil, not empty, when nothing was delivered.
func (s *Store) RecallWithHits(ctx context.Context, sc Scope, query string) (text string, hits []Delivered) {
	if s == nil {
		return "", nil
	}
	resp, scoredHits, err := s.recall(ctx, sc.Buckets(), query)
	if err != nil || resp == nil || len(resp.Memories) == 0 {
		return "", nil
	}
	text = formatMemories(resp.Memories)
	if text == "" {
		return "", nil
	}
	// scoredHits zips 1:1 with resp.Memories (recall's invariant), so the real cosine reaches the ledger.
	hits = make([]Delivered, 0, len(resp.Memories))
	for i, m := range resp.Memories {
		d := Delivered{ID: m.ID, Content: extractText(m), Tier: TierUnverified}
		if i < len(scoredHits) {
			d.Score = scoredHits[i].Score
			if scoredHits[i].Tier != "" {
				d.Tier = scoredHits[i].Tier
			}
		}
		hits = append(hits, d)
	}
	return fmt.Sprintf(recallInstructions, text), hits
}

// TopK is the configured recall size, the ceiling for recall_memory's k: a tool caller can narrow
// a recall but never widen it.
func (s *Store) TopK() int {
	if s == nil {
		return 0
	}
	return s.topK
}

// InjectionByteBudget bounds one recall's total Content bytes, shared with prefill so a tool call
// never injects more than prefill could.
const InjectionByteBudget = 8000

// CapForInjection drops tail hits once cumulative Content bytes exceed budget (<=0 = no cap),
// reporting whether anything was dropped.
func CapForInjection(hits []Delivered, budget int) (kept []Delivered, truncated bool) {
	if budget <= 0 {
		return hits, false
	}
	used := 0
	for i, h := range hits {
		used += len(h.Content)
		if used > budget {
			return hits[:i], true
		}
	}
	return hits, false
}

// RecallForTool is recall_memory's core: hits capped to k (<=0 or > top_k uses top_k) and then
// to InjectionByteBudget.
func (s *Store) RecallForTool(ctx context.Context, sc Scope, query string, k int) (hits []Delivered, truncated bool) {
	if s == nil {
		return nil, false
	}
	_, all := s.RecallWithHits(ctx, sc, query)
	if k > 0 && k < len(all) {
		all = all[:k]
	}
	return CapForInjection(all, InjectionByteBudget)
}

// FormatForModel renders recall_memory's citable id/tier/score/content list, plus a truncation notice.
func FormatForModel(hits []Delivered, truncated bool) string {
	if len(hits) == 0 {
		return "(no relevant memory found)"
	}
	var b strings.Builder
	for _, h := range hits {
		fmt.Fprintf(&b, "- id=%s tier=%s score=%.2f: %s\n", h.ID, h.Tier, h.Score, h.Content)
	}
	if truncated {
		b.WriteString("(truncated: more memories matched than fit the injection budget)\n")
	}
	return b.String()
}

// LogRecall appends a memory.recall ledger entry and bumps recalls/last_recalled_at, for callers
// with no round-scoped set to dedupe against (plan_judge).
func (s *Store) LogRecall(ctx context.Context, led ledger.LedgerStore, chatID, nodeID, source string, hits []Delivered) {
	if s == nil || len(hits) == 0 {
		return
	}
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.ID
	}
	if led != nil {
		s.appendRecallEntry(ctx, led, chatID, nodeID, source, hits)
	}
	s.RecordRecall(ctx, ids)
}

// LogRecallLedgerOnly logs for the audit trail but never bumps the counter; callers dedupe
// recalls against their own round's received set.
func (s *Store) LogRecallLedgerOnly(ctx context.Context, led ledger.LedgerStore, chatID, nodeID, source string, hits []Delivered) {
	if s == nil || led == nil || len(hits) == 0 {
		return
	}
	s.appendRecallEntry(ctx, led, chatID, nodeID, source, hits)
}

// appendRecallEntry writes LogRecall/LogRecallLedgerOnly's shared ledger side; failures
// are logged, never returned, since the append is observational.
func (s *Store) appendRecallEntry(ctx context.Context, led ledger.LedgerStore, chatID, nodeID, source string, hits []Delivered) {
	entries := make([]ledger.MemoryRecallEntry, len(hits))
	for i, h := range hits {
		entries[i] = ledger.MemoryRecallEntry{ID: h.ID, Score: h.Score}
	}
	payload, err := json.Marshal(ledger.MemoryRecallPayload{Source: source, Entries: entries})
	if err != nil {
		s.log.Warn("ledger memory.recall payload marshal failed (observational; call unaffected)", "chat_id", chatID, "node_id", nodeID, "err", err)
		return
	}
	if _, err := led.AppendIntent(ctx, ledger.Entry{
		ChatID: chatID, NodeID: nodeID, Kind: ledger.KindMemoryRecall, At: time.Now().UTC(), Payload: payload,
	}); err != nil {
		s.log.Warn("ledger memory.recall append failed (observational; call unaffected)", "chat_id", chatID, "node_id", nodeID, "err", err)
	}
}

// RecordRecall bumps recalls/last_recalled_at for ids in one batched write. Best-effort: failures are
// logged, never returned. No memory_ops row; the caller's memory.recall ledger entry is the audit trail.
func (s *Store) RecordRecall(ctx context.Context, ids []string) {
	if s == nil || len(ids) == 0 {
		return
	}
	if err := s.idx.recordRecall(ctx, ids); err != nil {
		s.log.Warn("record recall failed", "ids", ids, "err", err)
	}
}
