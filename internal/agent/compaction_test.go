package agent

import (
	"context"
	"iter"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// --- fakes -----------------------------------------------------------------

type fakeState struct{ m map[string]any }

func (s *fakeState) Get(k string) (any, error) {
	v, ok := s.m[k]
	if !ok {
		return nil, session.ErrStateKeyNotExist
	}
	return v, nil
}
func (s *fakeState) Set(k string, v any) error { s.m[k] = v; return nil }
func (s *fakeState) All() iter.Seq2[string, any] {
	return func(yield func(string, any) bool) {
		for k, v := range s.m {
			if !yield(k, v) {
				return
			}
		}
	}
}

// fakeCtx embeds StrictContextMock (v2 unified agent.Context) so it keeps
// satisfying the interface as it grows; we override only what compaction uses.
type fakeCtx struct {
	adkagent.StrictContextMock
	state *fakeState
}

func newFakeCtx() *fakeCtx {
	return &fakeCtx{StrictContextMock: adkagent.StrictContextMock{Ctx: context.Background()}, state: &fakeState{m: map[string]any{}}}
}

func (c *fakeCtx) UserContent() *genai.Content          { return nil }
func (c *fakeCtx) InvocationID() string                 { return "inv" }
func (c *fakeCtx) AgentName() string                    { return "test" }
func (c *fakeCtx) ReadonlyState() session.ReadonlyState { return c.state }
func (c *fakeCtx) UserID() string                       { return "u" }
func (c *fakeCtx) AppName() string                      { return "app" }
func (c *fakeCtx) SessionID() string                    { return "sess" }
func (c *fakeCtx) Branch() string                       { return "" }
func (c *fakeCtx) Artifacts() adkagent.Artifacts        { return nil }
func (c *fakeCtx) State() session.State                 { return c.state }

type fakeLLM struct {
	text       string
	calls      int
	lastPrompt string
}

func (f *fakeLLM) Name() string { return "fake" }
func (f *fakeLLM) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		f.calls++
		if len(req.Contents) > 0 && len(req.Contents[0].Parts) > 0 {
			f.lastPrompt = req.Contents[0].Parts[0].Text
		}
		yield(&model.LLMResponse{Content: &genai.Content{
			Role:  genai.RoleModel,
			Parts: []*genai.Part{{Text: f.text}},
		}}, nil)
	}
}

// --- builders --------------------------------------------------------------

func textContent(role, s string) *genai.Content {
	return &genai.Content{Role: role, Parts: []*genai.Part{{Text: s}}}
}

func toolCall(name, id string) *genai.Content {
	return &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{
		FunctionCall: &genai.FunctionCall{Name: name, ID: id, Args: map[string]any{"q": "x"}},
	}}}
}

func toolResult(name, id string, n int) *genai.Content {
	return &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{
		FunctionResponse: &genai.FunctionResponse{Name: name, ID: id, Response: map[string]any{"result": strings.Repeat("x", n)}},
	}}}
}

// --- tests -----------------------------------------------------------------

// Under budget: callback is a pure no-op and the summariser is never called.
func TestCompactionNoOpUnderBudget(t *testing.T) {
	llm := &fakeLLM{text: "S"}
	cb := compactionCallback(Compaction{Summarizer: llm, ContextWindow: 1_000_000, Prune: true, Enabled: true})
	req := &model.LLMRequest{Contents: []*genai.Content{textContent(genai.RoleUser, "task"), textContent(genai.RoleModel, "small answer")}}
	before := len(req.Contents)
	if _, err := cb(newFakeCtx(), req); err != nil {
		t.Fatalf("callback err: %v", err)
	}
	if llm.calls != 0 {
		t.Fatalf("summariser called %d times under budget; want 0", llm.calls)
	}
	if len(req.Contents) != before {
		t.Fatalf("contents changed under budget: %d → %d", before, len(req.Contents))
	}
}

// prune blanks old tool outputs, preserves the recent ones + call/response
// pairing (Name/ID), and is skipped when it wouldn't free enough.
func TestPrune(t *testing.T) {
	// 12 fetches of 40k chars (=10k tokens) each: 120k tokens total tool output.
	// Recent 40k tokens + last 2 messages protected; the rest (well over the 20k
	// minimum) gets blanked.
	const each = 40_000
	contents := []*genai.Content{textContent(genai.RoleUser, "task")}
	for i := 0; i < 12; i++ {
		contents = append(contents, toolCall("web_fetch", "c"))
		contents = append(contents, toolResult("web_fetch", "c", each))
	}
	freed := prune(contents)
	if freed <= 0 {
		t.Fatalf("prune freed %d tokens; expected it to engage", freed)
	}

	var blanked, intact int
	for _, c := range contents {
		for _, p := range c.Parts {
			fr := p.FunctionResponse
			if fr == nil {
				continue
			}
			if fr.Name == "" || fr.ID == "" {
				t.Fatalf("prune dropped Name/ID, breaking call/response pairing: %+v", fr)
			}
			if r, _ := fr.Response["result"].(string); r == prunedStub {
				blanked++
			} else {
				intact++
			}
		}
	}
	if blanked == 0 {
		t.Fatal("prune blanked nothing")
	}
	if intact == 0 {
		t.Fatal("prune blanked everything; recent output must be protected")
	}

	// Below the minimum: a single small old fetch shouldn't trigger a prune.
	small := []*genai.Content{
		textContent(genai.RoleUser, "task"),
		toolResult("web_fetch", "c", 1_000),
		textContent(genai.RoleModel, "a"),
		textContent(genai.RoleUser, "b"),
	}
	if freed := prune(small); freed != 0 {
		t.Fatalf("prune engaged on a sub-minimum gain: freed %d", freed)
	}
}

// Over budget after prune (all-text history): the head is summarised and the
// request is rebuilt as [task, summary, ...tail] within budget.
func TestCompactSummarises(t *testing.T) {
	llm := &fakeLLM{text: "## Goal\n- compacted"}
	// context_window 10k tokens, reserve 8k ⇒ usable 2k tokens (8k chars).
	cb := compactionCallback(Compaction{Summarizer: llm, ContextWindow: 25_000, Prune: true, Enabled: true})

	task := textContent(genai.RoleUser, "the self-contained task")
	contents := []*genai.Content{task}
	for i := 0; i < 20; i++ {
		contents = append(contents, textContent(genai.RoleModel, strings.Repeat("y", 2_000)))
	}
	req := &model.LLMRequest{Contents: contents}
	if _, err := cb(newFakeCtx(), req); err != nil {
		t.Fatalf("callback err: %v", err)
	}
	if llm.calls != 1 {
		t.Fatalf("summariser called %d times; want 1", llm.calls)
	}
	// The summary is merged into the task content (no separate inserted turn).
	parts := req.Contents[0].Parts
	if parts[0].Text != task.Parts[0].Text {
		t.Fatalf("task text not preserved as first part: %q", parts[0].Text)
	}
	if got := parts[len(parts)-1].Text; !strings.Contains(got, "compacted") {
		t.Fatalf("summary not appended to task content: %q", got)
	}
	if len(req.Contents) >= len(contents) {
		t.Fatalf("compaction did not shrink contents: %d → %d", len(contents), len(req.Contents))
	}
}

// splitHead never empties the tail or starts it on a dangling FunctionResponse:
// a recent oversized tool result is kept verbatim together with its call.
func TestSplitHeadKeepsTrailingToolResult(t *testing.T) {
	contents := []*genai.Content{textContent(genai.RoleUser, "task")}
	for i := 0; i < 8; i++ {
		contents = append(contents, textContent(genai.RoleModel, strings.Repeat("y", 2_000)))
	}
	contents = append(contents, toolCall("web_fetch", "c"))
	contents = append(contents, toolResult("web_fetch", "c", 40_000)) // bigger than preserve

	ts := splitHead(contents, 2_000)
	if ts <= 1 || ts >= len(contents) {
		t.Fatalf("tail empty or whole-history kept: ts=%d len=%d", ts, len(contents))
	}
	if hasFunctionResponse(contents[ts]) {
		t.Fatalf("tail starts with a dangling FunctionResponse at %d", ts)
	}
	if contents[ts].Parts[0].FunctionCall == nil {
		t.Fatal("tail should start at the FunctionCall matching the trailing result")
	}
	if contents[len(contents)-1].Parts[0].FunctionResponse == nil {
		t.Fatal("trailing tool result must remain in the verbatim tail")
	}
}

// Media bytes are not counted as tokens (so an image can't spuriously trigger
// compaction via the estimate).
func TestEstimateExcludesMedia(t *testing.T) {
	img := &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{
		InlineData: &genai.Blob{MIMEType: "image/png", Data: make([]byte, 4_000_000)},
	}}}
	if got := estimateTokens([]*genai.Content{img}); got > 10 {
		t.Fatalf("4MB image estimated at %d tokens; media bytes must be excluded", got)
	}
}

// recordUsage stores the provider's measured prompt tokens, and the callback
// triggers on that even when the chars/4 estimate is well under budget.
func TestMeasuredUsageTriggers(t *testing.T) {
	ctx := newFakeCtx()
	recordUsage()(ctx, &model.LLMResponse{
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 195_000},
	}, nil)
	if got := measuredInput(ctx); got != 195_000 {
		t.Fatalf("measuredInput = %d; want 195000", got)
	}

	llm := &fakeLLM{text: "## Goal\n- compacted"}
	// budget = 200000 - 8192 = 191808. Estimate stays far under; measured (195000) is over.
	cb := compactionCallback(Compaction{Summarizer: llm, ContextWindow: 200_000, Prune: false, Enabled: true})
	contents := []*genai.Content{textContent(genai.RoleUser, "task")}
	for i := 0; i < 20; i++ {
		contents = append(contents, textContent(genai.RoleModel, strings.Repeat("y", 3_000)))
	}
	req := &model.LLMRequest{Contents: contents}
	if est := estimateTokens(contents); est >= 191_808 {
		t.Fatalf("test precondition broken: estimate %d not under budget", est)
	}
	if _, err := cb(ctx, req); err != nil {
		t.Fatalf("callback err: %v", err)
	}
	if llm.calls != 1 {
		t.Fatalf("measured-usage trigger did not compact: summariser calls=%d", llm.calls)
	}
}

// The live-failure regression: recordUsage pairs the measured prompt tokens
// with the estimate stashed by the previous callback, and the resulting ratio
// makes the NEXT callback compact a request whose raw bytes/4 estimate is under
// budget but whose calibrated (real) size is over.
func TestCalibrationRecordedAndApplied(t *testing.T) {
	ctx := newFakeCtx()
	llm := &fakeLLM{text: "## Goal\n- compacted"}
	// budget = 60_000 - 20_000 = 40_000 real tokens.
	cb := compactionCallback(Compaction{Summarizer: llm, ContextWindow: 60_000, Prune: false, Enabled: true})

	// Turn 1: under budget, no-op — but the raw estimate (10_000) is stashed.
	small := &model.LLMRequest{Contents: []*genai.Content{textContent(genai.RoleUser, strings.Repeat("a", 40_000))}}
	if _, err := cb(ctx, small); err != nil {
		t.Fatalf("first callback err: %v", err)
	}
	if got := intState(ctx, estimateKey); got != 10_000 {
		t.Fatalf("estimate not stashed for calibration: got %d, want 10000", got)
	}
	// Provider measures 30_000 real prompt tokens (dense content + system prompt
	// + tool schemas the estimate can't see): ratio = 3.0.
	recordUsage()(ctx, &model.LLMResponse{
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 30_000},
	}, nil)
	if got := calibrationRatio(ctx); got != 3.0 {
		t.Fatalf("calibrationRatio = %v; want 3.0", got)
	}

	// Turn 2: raw estimate ~15_000 (under the 40_000 budget, so the old code
	// no-op'd and the provider 400'd) but calibrated ~45_000 is over → compacts.
	contents := []*genai.Content{textContent(genai.RoleUser, "task")}
	for i := 0; i < 20; i++ {
		contents = append(contents, textContent(genai.RoleModel, strings.Repeat("y", 3_000)))
	}
	req := &model.LLMRequest{Contents: contents}
	if est := estimateTokens(contents); est > 40_000 {
		t.Fatalf("test precondition broken: raw estimate %d not under budget", est)
	}
	if _, err := cb(ctx, req); err != nil {
		t.Fatalf("second callback err: %v", err)
	}
	if llm.calls != 1 {
		t.Fatalf("calibrated over-budget request did not compact: summariser calls=%d", llm.calls)
	}
}

// The post-prune "did we free enough?" check uses the calibrated value: a prune
// that brings the RAW estimate under budget but leaves the calibrated size over
// must fall through to summarisation instead of declaring victory.
func TestPostPruneCheckIsCalibrated(t *testing.T) {
	ctx := newFakeCtx()
	// Ratio 2.0: measured 20_000 vs stashed estimate 10_000.
	if err := ctx.state.Set(estimateKey, 10_000); err != nil {
		t.Fatal(err)
	}
	recordUsage()(ctx, &model.LLMResponse{
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 20_000},
	}, nil)

	llm := &fakeLLM{text: "## Goal\n- compacted"}
	// budget = 80_000 - 20_000 = 60_000 real tokens.
	cb := compactionCallback(Compaction{Summarizer: llm, ContextWindow: 80_000, Prune: true, Enabled: true})

	// 12 old tool fetches of 40k chars (10k est tokens) each ⇒ raw est ~120k.
	// prune protects the most recent ~40k est tokens and blanks the rest, landing
	// the raw estimate around 40k (< 60k budget) — but calibrated ~80k is over.
	contents := []*genai.Content{textContent(genai.RoleUser, "task")}
	for i := 0; i < 12; i++ {
		contents = append(contents, toolCall("web_fetch", "c"))
		contents = append(contents, toolResult("web_fetch", "c", 40_000))
	}
	for i := 0; i < 3; i++ {
		contents = append(contents, textContent(genai.RoleModel, "done with fetch batch"))
	}
	req := &model.LLMRequest{Contents: contents}
	if _, err := cb(ctx, req); err != nil {
		t.Fatalf("callback err: %v", err)
	}
	est := estimateTokens(req.Contents)
	if llm.calls != 1 {
		t.Fatalf("post-prune raw estimate %d passed the gate; calibrated check must summarise (calls=%d)", est, llm.calls)
	}
}

// Absurd measured/estimate ratios are clamped, and the ratio never drops below
// 1.0 (calibration must never shrink the raw estimate).
func TestCalibrationClamped(t *testing.T) {
	ctx := newFakeCtx()
	// Measured below estimate (media-free overcount): floor at 1.0.
	if err := ctx.state.Set(estimateKey, 10_000); err != nil {
		t.Fatal(err)
	}
	recordUsage()(ctx, &model.LLMResponse{
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 1_000},
	}, nil)
	if got := calibrationRatio(ctx); got != minCalibrationRatio {
		t.Fatalf("ratio %v below floor; want %v", got, minCalibrationRatio)
	}
	// Tiny request dominated by tool-schema overhead: ceiling caps it.
	if err := ctx.state.Set(estimateKey, 10); err != nil {
		t.Fatal(err)
	}
	recordUsage()(ctx, &model.LLMResponse{
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 100_000},
	}, nil)
	if got := calibrationRatio(ctx); got != maxCalibrationRatio {
		t.Fatalf("ratio %v above ceiling; want %v", got, maxCalibrationRatio)
	}
	// A zero estimate records no ratio at all (degenerate divide guarded).
	fresh := newFakeCtx()
	if err := fresh.state.Set(estimateKey, 0); err != nil {
		t.Fatal(err)
	}
	recordUsage()(fresh, &model.LLMResponse{
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 5_000},
	}, nil)
	if _, err := fresh.state.Get(calibrationKey); err == nil {
		t.Fatal("ratio recorded from a zero estimate")
	}
}

// First turn (no measurement yet): the conservative default ratio applies, and
// a comfortably under-budget request is still a pure no-op.
func TestCalibrationDefaultBeforeMeasurement(t *testing.T) {
	ctx := newFakeCtx()
	if got := calibrationRatio(ctx); got != defaultCalibrationRatio {
		t.Fatalf("calibrationRatio before measurement = %v; want default %v", got, defaultCalibrationRatio)
	}
	llm := &fakeLLM{text: "S"}
	cb := compactionCallback(Compaction{Summarizer: llm, ContextWindow: 1_000_000, Prune: true, Enabled: true})
	req := &model.LLMRequest{Contents: []*genai.Content{textContent(genai.RoleUser, "task")}}
	if _, err := cb(ctx, req); err != nil {
		t.Fatalf("callback err: %v", err)
	}
	if llm.calls != 0 || len(req.Contents) != 1 {
		t.Fatalf("first-turn under-budget request was not a no-op: calls=%d len=%d", llm.calls, len(req.Contents))
	}
}

// Once the anchored summary covers the older prefix, a later over-budget turn
// whose live tail still fits is served from the stored summary with NO new
// summariser call (fixes the "re-summarise every turn" cost).
func TestReuseSkipsSummariser(t *testing.T) {
	ctx := newFakeCtx()
	// budget = 200000 - 8192 = 191808; measured (195000) keeps the trigger firing.
	recordUsage()(ctx, &model.LLMResponse{
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 195_000},
	}, nil)

	llm := &fakeLLM{text: "## Goal\n- compacted"}
	cb := compactionCallback(Compaction{Summarizer: llm, ContextWindow: 200_000, Prune: false, Enabled: true})

	build := func() *model.LLMRequest {
		c := []*genai.Content{textContent(genai.RoleUser, "task")}
		for i := 0; i < 30; i++ {
			c = append(c, textContent(genai.RoleModel, strings.Repeat("y", 3_000)))
		}
		return &model.LLMRequest{Contents: c}
	}

	// First turn summarises once and records the coverage boundary.
	if _, err := cb(ctx, build()); err != nil {
		t.Fatalf("first callback err: %v", err)
	}
	if llm.calls != 1 {
		t.Fatalf("first turn: summariser calls=%d; want 1", llm.calls)
	}

	// Second turn (same grown session re-fed by ADK): reuse, no summariser call.
	req := build()
	if _, err := cb(ctx, req); err != nil {
		t.Fatalf("second callback err: %v", err)
	}
	if llm.calls != 1 {
		t.Fatalf("second turn re-summarised (calls=%d); should have reused the anchored summary", llm.calls)
	}
	parts := req.Contents[0].Parts
	if got := parts[len(parts)-1].Text; !strings.Contains(got, "compacted") {
		t.Fatalf("reuse did not reapply the summary to the task content: %q", got)
	}
	if len(req.Contents) >= len(build().Contents) {
		t.Fatalf("reuse did not shrink contents: %d", len(req.Contents))
	}
}

// The anchored summary is persisted to state and fed back as <previous-summary>
// on the next compaction.
func TestAnchoredSummaryFedBack(t *testing.T) {
	llm := &fakeLLM{text: "FIRST-SUMMARY"}
	// usable = ctx - min(MaxOutputTokens, compactionBuffer=20000) = 1808: a small
	// budget so the second oversized turn re-summarises (not the reuse fast-path).
	cb := compactionCallback(Compaction{Summarizer: llm, ContextWindow: 21_808, Prune: false, Enabled: true})
	ctx := newFakeCtx()

	oversized := func() *model.LLMRequest {
		c := []*genai.Content{textContent(genai.RoleUser, "task")}
		for i := 0; i < 20; i++ {
			c = append(c, textContent(genai.RoleModel, strings.Repeat("z", 2_000)))
		}
		return &model.LLMRequest{Contents: c}
	}

	if _, err := cb(ctx, oversized()); err != nil {
		t.Fatalf("first callback err: %v", err)
	}
	if got, _ := ctx.state.Get(summaryStateKey); got != "FIRST-SUMMARY" {
		t.Fatalf("summary not anchored to state: %v", got)
	}

	llm.text = "SECOND-SUMMARY"
	if _, err := cb(ctx, oversized()); err != nil {
		t.Fatalf("second callback err: %v", err)
	}
	if !strings.Contains(llm.lastPrompt, "<previous-summary>") || !strings.Contains(llm.lastPrompt, "FIRST-SUMMARY") {
		t.Fatalf("second summarise did not receive the anchored previous summary; prompt:\n%s", llm.lastPrompt)
	}
}
