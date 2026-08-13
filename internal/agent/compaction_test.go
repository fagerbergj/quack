package agent

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/stream"
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
	// usage is nil by default (existing callers get no usage metadata, matching prior behaviour).
	usage *genai.GenerateContentResponseUsageMetadata
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
		}, UsageMetadata: f.usage}, nil)
	}
}

// failingLLM always errors - for exercising the "summariser call failed"
// skip-and-continue path.
type failingLLM struct{ calls int }

func (f *failingLLM) Name() string { return "failing" }
func (f *failingLLM) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		f.calls++
		yield(nil, fmt.Errorf("summariser unavailable"))
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

// sentinelIndex returns the index of the durable summary content in contents,
// or -1.
func sentinelIndex(contents []*genai.Content) int {
	for i, c := range contents {
		if isSentinel(c) {
			return i
		}
	}
	return -1
}

// budgetWindow returns a ContextWindow whose usable() threshold sits strictly
// between the calibrated size of ALL of contents and the calibrated size of
// just its last `retention` items - enough to force exactly one compaction
// round whose resulting tail already fits under budget (no backstop clamp
// ladder needed). Requires len(contents) > retention+1.
func budgetWindow(contents []*genai.Content, retention int) int {
	all := calibrated(estimateTokens(contents), defaultCalibrationRatio, 0)
	tailStart := len(contents) - retention
	tail := calibrated(estimateTokens(contents[tailStart:]), defaultCalibrationRatio, 0)
	return (all+tail)/2 + compactionBuffer
}

func newSessions(t *testing.T, ctx *fakeCtx) (session.Service, session.Session) {
	t.Helper()
	sessions := session.InMemoryService()
	resp, err := sessions.Create(ctx, &session.CreateRequest{AppName: ctx.AppName(), UserID: ctx.UserID(), SessionID: ctx.SessionID()})
	if err != nil {
		t.Fatalf("session create: %v", err)
	}
	return sessions, resp.Session
}

// --- tests -----------------------------------------------------------------

// Under budget: callback is a pure no-op and the summariser is never called.
func TestCompactionNoOpUnderBudget(t *testing.T) {
	llm := &fakeLLM{text: "S"}
	cb := compactionCallback(Compaction{Summarizer: llm, ContextWindow: 1_000_000, Enabled: true})
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

// THE invariant: after compaction the model must never see a tool call whose
// result has been replaced by a placeholder. Either the turn is gone (its
// knowledge folded into the summary) or it is intact. A blanked-in-place result
// ("[earlier tool output elided…]") is amnesia: the model can see it read the
// file and that the content is gone, so it reads it again (live churn: the same
// file read 8x, list_dir 9x in one node).
func TestNoBlankedToolResultsSurviveCompaction(t *testing.T) {
	llm := &fakeLLM{text: "## Goal\n- compacted"}

	// 12 old tool fetches of 40k chars (10k est tokens) each.
	contents := []*genai.Content{textContent(genai.RoleUser, "task")}
	for i := 0; i < 12; i++ {
		contents = append(contents, toolCall("read_file", "c"))
		contents = append(contents, toolResult("read_file", "c", 40_000))
	}

	// A threshold strictly between "everything" and "just the retained tail":
	// compaction must fire, but the retained tail (EventRetentionSize contents)
	// fits verbatim on its own - no backstop clamp ladder needed.
	cb := compactionCallback(Compaction{Summarizer: llm, ContextWindow: budgetWindow(contents, defaultEventRetentionSize), Enabled: true})
	req := &model.LLMRequest{Contents: contents}
	if _, err := cb(newFakeCtx(), req); err != nil {
		t.Fatalf("callback err: %v", err)
	}

	intact := strings.Repeat("x", 40_000) // what toolResult puts in Response["result"]
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			fr := p.FunctionResponse
			if fr == nil {
				continue
			}
			if got, _ := fr.Response["result"].(string); got != intact {
				t.Fatalf("a surviving tool result was replaced by a placeholder (%q); the turn must be dropped and summarised, not hollowed out", truncate(got, 120))
			}
		}
	}
	// And the knowledge it carried must have been summarised, not just discarded.
	if llm.calls != 1 {
		t.Fatalf("older tool outputs were discarded without summarising them: summariser calls=%d; want 1", llm.calls)
	}
}

// Over budget (all-text history): the head is summarised into a durable
// summary content, and the request is rebuilt as [task, summary, ...tail]
// within budget.
func TestCompactSummarises(t *testing.T) {
	llm := &fakeLLM{text: "## Goal\n- compacted"}

	task := textContent(genai.RoleUser, "the self-contained task")
	contents := []*genai.Content{task}
	for i := 0; i < 30; i++ {
		contents = append(contents, textContent(genai.RoleModel, strings.Repeat("y", 2_000)))
	}
	cb := compactionCallback(Compaction{Summarizer: llm, ContextWindow: budgetWindow(contents, defaultEventRetentionSize), Enabled: true})

	req := &model.LLMRequest{Contents: append([]*genai.Content{}, contents...)}
	if _, err := cb(newFakeCtx(), req); err != nil {
		t.Fatalf("callback err: %v", err)
	}
	if llm.calls != 1 {
		t.Fatalf("summariser called %d times; want 1", llm.calls)
	}
	if req.Contents[0].Parts[0].Text != task.Parts[0].Text {
		t.Fatalf("task text not preserved verbatim at contents[0]: %q", req.Contents[0].Parts[0].Text)
	}
	idx := sentinelIndex(req.Contents)
	if idx != 1 {
		t.Fatalf("expected the durable summary content at index 1, got sentinel index %d", idx)
	}
	if got := req.Contents[idx].Parts[0].Text; !strings.Contains(got, "compacted") {
		t.Fatalf("summary content missing summariser output: %q", got)
	}
	if len(req.Contents) >= len(contents) {
		t.Fatalf("compaction did not shrink contents: %d → %d", len(contents), len(req.Contents))
	}
}

// TestCompactionEmitsNodeScopedEvent: a compaction round must forward a
// compaction event through the yield-ctx escape hatch, carrying the node/run
// coordinates and the actual before/after shrink - so the frontend's context
// meter and node timeline see it, not just the log line.
func TestCompactionEmitsNodeScopedEvent(t *testing.T) {
	llm := &fakeLLM{text: "## Goal\n- compacted"}

	task := textContent(genai.RoleUser, "the self-contained task")
	contents := []*genai.Content{task}
	for i := 0; i < 30; i++ {
		contents = append(contents, textContent(genai.RoleModel, strings.Repeat("y", 2_000)))
	}
	cb := compactionCallback(Compaction{Summarizer: llm, ContextWindow: budgetWindow(contents, defaultEventRetentionSize), Enabled: true})

	var got []stream.SSEEvent
	ctx := newFakeCtx()
	ctx.Ctx = ledger.WithCoords(
		stream.WithYield(context.Background(), func(ev stream.SSEEvent) { got = append(got, ev) }),
		ledger.Coords{Node: "n1", Round: "worker-r0"},
	)

	req := &model.LLMRequest{Contents: append([]*genai.Content{}, contents...)}
	if _, err := cb(ctx, req); err != nil {
		t.Fatalf("callback err: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("compaction events emitted = %d, want 1: %+v", len(got), got)
	}
	if got[0].Name != stream.EventCompaction {
		t.Fatalf("event name = %q, want %q", got[0].Name, stream.EventCompaction)
	}
	d := got[0].Data.(stream.CompactionData)
	if d.NodeID != "n1" || d.RunID != "worker-r0" {
		t.Fatalf("compaction event coords = %+v, want node=n1 run=worker-r0", d)
	}
	if d.TokensBefore <= d.TokensAfter {
		t.Fatalf("compaction event tokens_before=%d tokens_after=%d, want a real shrink", d.TokensBefore, d.TokensAfter)
	}
}

// TestCompactionNoYieldNoPanic: outside a DAG node run (no yield-ctx attached
// - unit tests, the advisor's own nested runner), compaction must still work;
// emitting the event is best-effort, never a hard dependency.
func TestCompactionNoYieldNoPanic(t *testing.T) {
	llm := &fakeLLM{text: "## Goal\n- compacted"}
	contents := []*genai.Content{textContent(genai.RoleUser, "task")}
	for i := 0; i < 30; i++ {
		contents = append(contents, textContent(genai.RoleModel, strings.Repeat("y", 2_000)))
	}
	cb := compactionCallback(Compaction{Summarizer: llm, ContextWindow: budgetWindow(contents, defaultEventRetentionSize), Enabled: true})
	req := &model.LLMRequest{Contents: contents}
	if _, err := cb(newFakeCtx(), req); err != nil {
		t.Fatalf("callback err: %v", err)
	}
	if llm.calls != 1 {
		t.Fatalf("compaction did not run without a yield sink: calls=%d", llm.calls)
	}
}

// TestSummarizeHead_DefaultAgentFillsTokenUsage pins serve.go's compaction
// fallback wiring: ResolveSummarizer only reaches fallbackSummarizer when no
// active worker model is available - a call site outside any node's own coords
// stamp - so the fallback's tracedModel needs the SetDefaultAgent("compaction")
// fallback to attribute its token usage at all.
func TestSummarizeHead_DefaultAgentFillsTokenUsage(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	if err := otelobs.InitMetricsForTesting(mp.Meter("test")); err != nil {
		t.Fatalf("InitMetricsForTesting: %v", err)
	}

	fallback := inference.TracedModelForTesting(&fakeLLM{
		text:  "summary",
		usage: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 10, CandidatesTokenCount: 5},
	}, "compaction-fallback-test-model")
	if da, ok := fallback.(interface{ SetDefaultAgent(string) }); ok {
		da.SetDefaultAgent("compaction")
	} else {
		t.Fatal("TracedModelForTesting result does not implement SetDefaultAgent")
	}

	// No ledger coords on ctx - mirrors ResolveSummarizer's fallback call site.
	if _, err := summarizeHead(context.Background(), fallback, "summarize this"); err != nil {
		t.Fatalf("summarizeHead: %v", err)
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	var found bool
	for _, sm := range rm.ScopeMetrics {
		for _, met := range sm.Metrics {
			if met.Name != "gen_ai.client.token.usage" {
				continue
			}
			sum, ok := met.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("gen_ai.client.token.usage is not an int64 Sum")
			}
			for _, dp := range sum.DataPoints {
				agentVal, _ := dp.Attributes.Value(attribute.Key("agent"))
				if agentVal.AsString() == "compaction" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("no gen_ai.client.token.usage data point carries agent=compaction - the fallback summariser's SetDefaultAgent never reached the metric")
	}
}

// longestSelfContainedPrefix must never end between a FunctionCall and its
// matching FunctionResponse - a recent tool round-trip stays paired, never
// split by the boundary (port of splitHead's dangling-response walk).
func TestLongestSelfContainedPrefixKeepsCallAndResponsePaired(t *testing.T) {
	contents := []*genai.Content{textContent(genai.RoleUser, "task")}
	for i := 0; i < 8; i++ {
		contents = append(contents, textContent(genai.RoleModel, strings.Repeat("y", 2_000)))
	}
	contents = append(contents, toolCall("web_fetch", "c"))
	contents = append(contents, toolResult("web_fetch", "c", 40_000))

	// A window ending right after the call (before its response) must not be
	// reported as balanced there.
	if n := longestSelfContainedPrefix(contents[1 : len(contents)-1]); n >= len(contents)-2 {
		t.Fatalf("prefix included the dangling call without its response: n=%d", n)
	}
	// The full range, including the matching response, is balanced end to end.
	if got := longestSelfContainedPrefix(contents[1:]); got != len(contents)-1 {
		t.Fatalf("full range including the matched response should be fully balanced: got %d, want %d", got, len(contents)-1)
	}
}

// boundary must never cut between an open call and its response even when the
// count-based retention target alone would land exactly there - the pair is
// pulled into the tail together instead.
func TestBoundaryNeverSplitsAnOpenCall(t *testing.T) {
	contents := []*genai.Content{textContent(genai.RoleUser, "task")}
	for i := 0; i < 8; i++ {
		contents = append(contents, textContent(genai.RoleModel, strings.Repeat("y", 2_000)))
	}
	contents = append(contents, toolCall("web_fetch", "c"))
	contents = append(contents, toolResult("web_fetch", "c", 40_000))

	headEnd, ok := boundary(contents, 1) // retention=1 would, by count alone, cut right between the call and response
	if !ok {
		t.Fatalf("boundary found no safe cut at all")
	}
	if headEnd == len(contents)-1 {
		t.Fatalf("boundary cut between the call (%d) and its response (%d), splitting them", len(contents)-2, len(contents)-1)
	}
}

// Boundary: every candidate split leaves a FunctionCall open (its response
// never appears within the window) → compaction is SKIPPED entirely this
// round (port of test_token_threshold_excludes_pending_function_call_events).
func TestBoundarySkippedWhenEveryCandidateSplitIsOpen(t *testing.T) {
	llm := &fakeLLM{text: "must not be called"}
	contents := []*genai.Content{textContent(genai.RoleUser, "task")}
	contents = append(contents, toolCall("long_running_op", "c1")) // no matching response anywhere
	for i := 0; i < 30; i++ {
		contents = append(contents, textContent(genai.RoleModel, strings.Repeat("y", 2_000)))
	}
	cb := compactionCallback(Compaction{Summarizer: llm, ContextWindow: 25_000, Enabled: true})

	req := &model.LLMRequest{Contents: append([]*genai.Content{}, contents...)}
	if _, err := cb(newFakeCtx(), req); err != nil {
		t.Fatalf("callback err: %v", err)
	}
	if llm.calls != 0 {
		t.Fatalf("summariser called %d times; compaction should have been skipped entirely (every split leaves the call open)", llm.calls)
	}
	if sentinelIndex(req.Contents) >= 0 {
		t.Fatalf("a summary was appended despite no safe cut existing this round")
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

// A media part inside the summarised head must render as a placeholder in the
// summariser's prompt, not vanish silently.
func TestMediaPlaceholderInSummaryPrompt(t *testing.T) {
	llm := &fakeLLM{text: "## Goal\n- compacted"}
	cb := compactionCallback(Compaction{Summarizer: llm, ContextWindow: 25_000, Enabled: true})

	contents := []*genai.Content{textContent(genai.RoleUser, "task")}
	contents = append(contents, &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{
		{InlineData: &genai.Blob{MIMEType: "image/png", Data: make([]byte, 1024)}},
	}})
	for i := 0; i < 30; i++ {
		contents = append(contents, textContent(genai.RoleModel, strings.Repeat("y", 2_000)))
	}
	req := &model.LLMRequest{Contents: contents}
	if _, err := cb(newFakeCtx(), req); err != nil {
		t.Fatalf("callback err: %v", err)
	}
	if llm.calls != 1 {
		t.Fatalf("summariser calls=%d; want 1", llm.calls)
	}
	if !strings.Contains(llm.lastPrompt, "[media: image/png omitted]") {
		t.Fatalf("summariser prompt does not carry a media placeholder; media vanished silently:\n%s", llm.lastPrompt)
	}
}

// A failed summarise must not block the model call, must not append a durable
// event, and must leave the request otherwise intact (existing behavior).
func TestSummariserFailureSkipsAppendAndContinues(t *testing.T) {
	ctx := newFakeCtx()
	sessions, _ := newSessions(t, ctx)

	failing := &failingLLM{}
	cb := compactionCallback(Compaction{Summarizer: failing, Sessions: sessions, ContextWindow: 25_000, Enabled: true})

	contents := []*genai.Content{textContent(genai.RoleUser, "task")}
	for i := 0; i < 30; i++ {
		contents = append(contents, textContent(genai.RoleModel, strings.Repeat("y", 2_000)))
	}
	req := &model.LLMRequest{Contents: contents}
	if _, err := cb(ctx, req); err != nil {
		t.Fatalf("callback must not error on a failed summarise: %v", err)
	}
	if failing.calls != 1 {
		t.Fatalf("summariser calls=%d; want 1", failing.calls)
	}
	if sentinelIndex(req.Contents) >= 0 {
		t.Fatalf("a summary was appended despite the summariser failing")
	}
	resp, err := sessions.Get(ctx, &session.GetRequest{AppName: ctx.AppName(), UserID: ctx.UserID(), SessionID: ctx.SessionID()})
	if err != nil {
		t.Fatalf("session get: %v", err)
	}
	if n := resp.Session.Events().Len(); n != 0 {
		t.Fatalf("%d events appended despite the summariser failing; want 0", n)
	}
}

// Sessions == nil must not panic: the in-request view still compacts (and the
// summary still benefits THIS request), it just isn't durably persisted.
func TestNilSessionsStillCompactsRequestWithoutPanic(t *testing.T) {
	llm := &fakeLLM{text: "## Goal\n- compacted"}
	cb := compactionCallback(Compaction{Summarizer: llm, ContextWindow: 25_000, Enabled: true}) // Sessions left nil

	contents := []*genai.Content{textContent(genai.RoleUser, "task")}
	for i := 0; i < 30; i++ {
		contents = append(contents, textContent(genai.RoleModel, strings.Repeat("y", 2_000)))
	}
	req := &model.LLMRequest{Contents: contents}
	if _, err := cb(newFakeCtx(), req); err != nil {
		t.Fatalf("callback err with nil Sessions: %v", err)
	}
	if llm.calls != 1 {
		t.Fatalf("summariser calls=%d; want 1 (nil Sessions must still allow the in-request view to compact)", llm.calls)
	}
	if sentinelIndex(req.Contents) < 0 {
		t.Fatalf("no sentinel in the filtered view despite a successful summarise")
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
	cb := compactionCallback(Compaction{Summarizer: llm, ContextWindow: 200_000, Enabled: true})
	threshold := usable(200_000)
	contents := []*genai.Content{textContent(genai.RoleUser, "task")}
	for i := 0; i < 30; i++ {
		contents = append(contents, textContent(genai.RoleModel, strings.Repeat("y", 3_000)))
	}
	req := &model.LLMRequest{Contents: contents}
	if est := estimateTokens(contents); est >= threshold {
		t.Fatalf("test precondition broken: estimate %d not under threshold %d", est, threshold)
	}
	if _, err := cb(ctx, req); err != nil {
		t.Fatalf("callback err: %v", err)
	}
	if llm.calls != 1 {
		t.Fatalf("measured-usage trigger did not compact: summariser calls=%d", llm.calls)
	}
}

// The "does it fit?" check uses the calibrated value, not the raw bytes/4
// estimate: a tool-heavy history whose raw estimate is under budget but whose
// calibrated (real) size is over must still be summarised, and the request must
// come back under budget.
func TestOverBudgetCheckIsCalibrated(t *testing.T) {
	ctx := newFakeCtx()
	// Ratio 2.0: measured 20_000 vs stashed estimate 10_000.
	if err := ctx.state.Set(estimateKey, 10_000); err != nil {
		t.Fatal(err)
	}
	recordUsage()(ctx, &model.LLMResponse{
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 20_000},
	}, nil)

	llm := &fakeLLM{text: "## Goal\n- compacted"}
	// threshold = 80_000 - 20_000 = 60_000 real tokens.
	cb := compactionCallback(Compaction{Summarizer: llm, ContextWindow: 80_000, Enabled: true})

	// 12 old tool fetches of 40k chars (10k est tokens) each ⇒ raw est ~120k.
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
	if llm.calls != 1 {
		t.Fatalf("calibrated over-budget request did not summarise (calls=%d)", llm.calls)
	}
	if got := calibrated(estimateTokens(req.Contents), defaultCalibrationRatio, 0); got > 60_000 {
		t.Fatalf("request still over budget after compaction: calibrated %d > 60000", got)
	}
}

// The summary is the only thing the agent keeps of the compacted turns, so the
// summariser must be asked for the knowledge that stops it redoing work (goose's
// Files+Code / commands / errors sections), and the model must be TOLD its
// context was compacted rather than left to think it is starting fresh.
func TestSummaryCarriesCodeKnowledge(t *testing.T) {
	llm := &fakeLLM{text: "## Goal\n- compacted"}
	cb := compactionCallback(Compaction{Summarizer: llm, ContextWindow: 25_000, Enabled: true})

	contents := []*genai.Content{textContent(genai.RoleUser, "task")}
	for i := 0; i < 15; i++ {
		contents = append(contents, toolCall("read_file", "c"))
		contents = append(contents, toolResult("read_file", "c", 4_000))
	}
	req := &model.LLMRequest{Contents: contents}
	if _, err := cb(newFakeCtx(), req); err != nil {
		t.Fatalf("callback err: %v", err)
	}
	if llm.calls != 1 {
		t.Fatalf("summariser not called: calls=%d", llm.calls)
	}
	for _, want := range []string{"Files & Code State", "Commands & Tools Run", "Errors & Fixes", "Repository State"} {
		if !strings.Contains(llm.lastPrompt, want) {
			t.Fatalf("summariser prompt does not ask for %q - the knowledge whose absence makes the agent re-read a file", want)
		}
	}
	idx := sentinelIndex(req.Contents)
	if idx < 0 {
		t.Fatalf("no durable summary content in the request")
	}
	if got := req.Contents[idx].Parts[0].Text; !strings.Contains(got, "Your context was compacted") {
		t.Fatalf("model not told its context was compacted: %q", got)
	}
}

// The contents[0] backstop: an oversized self-contained task/revise prompt -
// which summarisation leaves verbatim - is middle-truncated to fit,
// so contents[0] alone can never strand the session on a 400 (the revise-round
// spiral). Non-text parts are preserved; the head keeps a usable core.
func TestTruncateOversizedHead(t *testing.T) {
	llm := &fakeLLM{text: "S"}
	// threshold = 60_000 - 20_000 = 40_000 real tokens; default ratio 1.3.
	cb := compactionCallback(Compaction{Summarizer: llm, ContextWindow: 60_000, Enabled: true})

	// A single huge task content (~50k tokens estimate ⇒ ~65k calibrated) with no
	// tail to summarise: only the head backstop can save it.
	huge := strings.Repeat("x", 200_000) // 50k est tokens
	img := &genai.Part{InlineData: &genai.Blob{MIMEType: "image/png", Data: make([]byte, 1024)}}
	req := &model.LLMRequest{Contents: []*genai.Content{{
		Role:  genai.RoleUser,
		Parts: []*genai.Part{{Text: huge}, img},
	}}}
	if _, err := cb(newFakeCtx(), req); err != nil {
		t.Fatalf("callback err: %v", err)
	}
	if len(req.Contents) != 1 {
		t.Fatalf("expected the single task content, got %d", len(req.Contents))
	}
	got := estimateTokens(req.Contents)
	if calibrated(got, defaultCalibrationRatio, 0) > usable(60_000) {
		t.Fatalf("oversized head not truncated under budget: calibrated %d > %d", calibrated(got, defaultCalibrationRatio, 0), usable(60_000))
	}
	text := req.Contents[0].Parts[0].Text
	if !strings.Contains(text, "elided to fit the context window") {
		t.Fatalf("truncation marker missing from truncated head")
	}
	if !strings.HasPrefix(text, "xxxx") {
		t.Fatalf("head core not preserved")
	}
	// The media part survives the collapse.
	var hasImg bool
	for _, p := range req.Contents[0].Parts {
		if p.InlineData != nil {
			hasImg = true
		}
	}
	if !hasImg {
		t.Fatalf("non-text (media) part dropped by head truncation")
	}
}

// First turn (no measurement yet): the conservative default ratio applies, and
// a comfortably under-budget request is still a pure no-op.
func TestCalibrationDefaultBeforeMeasurement(t *testing.T) {
	ctx := newFakeCtx()
	llm := &fakeLLM{text: "S"}
	cb := compactionCallback(Compaction{Summarizer: llm, ContextWindow: 1_000_000, Enabled: true})
	req := &model.LLMRequest{Contents: []*genai.Content{textContent(genai.RoleUser, "task")}}
	if _, err := cb(ctx, req); err != nil {
		t.Fatalf("callback err: %v", err)
	}
	if llm.calls != 0 || len(req.Contents) != 1 {
		t.Fatalf("first-turn under-budget request was not a no-op: calls=%d len=%d", llm.calls, len(req.Contents))
	}
}

// Once a summary event is durably appended, a later request that already
// carries it (ADK's real request-builder folds every session event into
// req.Contents by itself) is served by VIEW FILTERING alone - no new
// summariser call (the reuse path; the whole point of the ADK port is a
// stable prompt-cache prefix across turns like this one).
func TestReuseSkipsSummariser(t *testing.T) {
	ctx := newFakeCtx()
	sessions, _ := newSessions(t, ctx)

	llm := &fakeLLM{text: "## Goal\n- compacted"}
	build := func() []*genai.Content {
		c := []*genai.Content{textContent(genai.RoleUser, "task")}
		for i := 0; i < 30; i++ {
			c = append(c, textContent(genai.RoleModel, strings.Repeat("y", 3_000)))
		}
		return c
	}
	cb := compactionCallback(Compaction{Summarizer: llm, Sessions: sessions, ContextWindow: budgetWindow(build(), defaultEventRetentionSize), Enabled: true})

	// First turn: over threshold, summarises once.
	req := &model.LLMRequest{Contents: build()}
	if _, err := cb(ctx, req); err != nil {
		t.Fatalf("first callback err: %v", err)
	}
	if llm.calls != 1 {
		t.Fatalf("first turn: summariser calls=%d; want 1", llm.calls)
	}
	idx := sentinelIndex(req.Contents)
	if idx < 0 {
		t.Fatalf("no durable summary content after first compaction")
	}
	firstSummary := req.Contents[idx].Parts[0].Text

	// Second turn: the session grew by one more small turn since the durable
	// summary was appended - exactly what ADK's real request-builder would
	// hand back (the summary event flows into req.Contents by itself).
	grown := append(append([]*genai.Content{}, req.Contents...), textContent(genai.RoleModel, "one more turn"))
	req2 := &model.LLMRequest{Contents: grown}
	if _, err := cb(ctx, req2); err != nil {
		t.Fatalf("second callback err: %v", err)
	}
	if llm.calls != 1 {
		t.Fatalf("second turn re-summarised (calls=%d); should have reused the durable summary via view filtering", llm.calls)
	}
	idx2 := sentinelIndex(req2.Contents)
	if idx2 < 0 || req2.Contents[idx2].Parts[0].Text != firstSummary {
		t.Fatalf("reuse did not carry the same durable summary forward")
	}
}

// Rolling summaries: a second firing seeds the summariser with the FIRST
// summary (present in the request as the prior sentinel content) - and
// filtering keys on the LAST sentinel, so the old summary event goes inert in
// the log (subsumption for free) even though it is never deleted.
func TestAnchoredSummaryFedBack(t *testing.T) {
	ctx := newFakeCtx()
	sessions, _ := newSessions(t, ctx)

	llm := &fakeLLM{text: "FIRST-SUMMARY"}
	build := func() []*genai.Content {
		c := []*genai.Content{textContent(genai.RoleUser, "task")}
		for i := 0; i < 30; i++ {
			c = append(c, textContent(genai.RoleModel, strings.Repeat("z", 2_000)))
		}
		return c
	}
	cb := compactionCallback(Compaction{Summarizer: llm, Sessions: sessions, ContextWindow: budgetWindow(build(), defaultEventRetentionSize), Enabled: true})

	req := &model.LLMRequest{Contents: build()}
	if _, err := cb(ctx, req); err != nil {
		t.Fatalf("first callback err: %v", err)
	}
	if llm.calls != 1 {
		t.Fatalf("first turn did not summarise: calls=%d", llm.calls)
	}
	idx := sentinelIndex(req.Contents)
	if idx < 0 || !strings.Contains(req.Contents[idx].Parts[0].Text, "FIRST-SUMMARY") {
		t.Fatalf("first summary not durably present: %+v", req.Contents)
	}

	// Pile on another full batch of turns - comfortably past what the same
	// threshold can hold - forcing a SECOND compaction.
	llm.text = "SECOND-SUMMARY"
	grown := append(append([]*genai.Content{}, req.Contents...), build()[1:]...)
	req2 := &model.LLMRequest{Contents: grown}
	if _, err := cb(ctx, req2); err != nil {
		t.Fatalf("second callback err: %v", err)
	}
	if llm.calls != 2 {
		t.Fatalf("second turn did not re-summarise: calls=%d", llm.calls)
	}
	if !strings.Contains(llm.lastPrompt, "<previous-summary>") || !strings.Contains(llm.lastPrompt, "FIRST-SUMMARY") {
		t.Fatalf("second summarise did not receive the durable previous summary; prompt:\n%s", llm.lastPrompt)
	}
	idx2 := sentinelIndex(req2.Contents)
	if idx2 < 0 || !strings.Contains(req2.Contents[idx2].Parts[0].Text, "SECOND-SUMMARY") {
		t.Fatalf("second summary not present in the durable view: %+v", req2.Contents)
	}

	// The first summary event is superseded, not deleted - raw events are
	// never mutated or removed (ADK invariant).
	resp, err := sessions.Get(ctx, &session.GetRequest{AppName: ctx.AppName(), UserID: ctx.UserID(), SessionID: ctx.SessionID()})
	if err != nil {
		t.Fatalf("session get: %v", err)
	}
	var sentinels int
	for ev := range resp.Session.Events().All() {
		if ev.Content != nil && isSentinel(ev.Content) {
			sentinels++
		}
	}
	if sentinels != 2 {
		t.Fatalf("durable log has %d sentinel events; want 2 (both compactions survive, the old one inert)", sentinels)
	}
}

// Over-threshold history durably appends exactly ONE summary event (author
// model, sentinel prefix), the request becomes [task, summary, retained
// tail] with the tail sized to EventRetentionSize, and every original session
// event survives untouched (log-preserving, ADK invariant).
func TestDurableSummaryEventAppended(t *testing.T) {
	ctx := newFakeCtx()
	sessions, sess := newSessions(t, ctx)

	contents := []*genai.Content{textContent(genai.RoleUser, "the task")}
	for i := 0; i < 30; i++ {
		contents = append(contents, textContent(genai.RoleModel, strings.Repeat("y", 3_000)))
	}
	// Persist the turns as REAL session events first, as ADK's own runtime
	// would have - proves compaction only ever APPENDS, never deletes.
	for _, c := range contents {
		ev := session.NewEvent(ctx, ctx.InvocationID())
		ev.Author = c.Role
		ev.Content = c
		if err := sessions.AppendEvent(ctx, sess, ev); err != nil {
			t.Fatalf("seed append: %v", err)
		}
	}
	before, err := sessions.Get(ctx, &session.GetRequest{AppName: ctx.AppName(), UserID: ctx.UserID(), SessionID: ctx.SessionID()})
	if err != nil {
		t.Fatalf("session get: %v", err)
	}
	beforeIDs := map[string]bool{}
	for ev := range before.Session.Events().All() {
		beforeIDs[ev.ID] = true
	}
	if len(beforeIDs) != len(contents) {
		t.Fatalf("seed: %d events persisted, want %d", len(beforeIDs), len(contents))
	}

	llm := &fakeLLM{text: "## Goal\n- compacted"}
	cb := compactionCallback(Compaction{Summarizer: llm, Sessions: sessions, ContextWindow: budgetWindow(contents, defaultEventRetentionSize), Enabled: true})

	req := &model.LLMRequest{Contents: append([]*genai.Content{}, contents...)}
	if _, err := cb(ctx, req); err != nil {
		t.Fatalf("callback err: %v", err)
	}
	if llm.calls != 1 {
		t.Fatalf("summariser calls=%d; want 1", llm.calls)
	}

	if req.Contents[0].Parts[0].Text != contents[0].Parts[0].Text {
		t.Fatalf("task not preserved verbatim at contents[0]")
	}
	if !isSentinel(req.Contents[1]) {
		t.Fatalf("contents[1] is not the durable summary: %+v", req.Contents[1])
	}
	if got := len(req.Contents) - 2; got != defaultEventRetentionSize {
		t.Fatalf("retained tail = %d contents; want EventRetentionSize %d", got, defaultEventRetentionSize)
	}

	after, err := sessions.Get(ctx, &session.GetRequest{AppName: ctx.AppName(), UserID: ctx.UserID(), SessionID: ctx.SessionID()})
	if err != nil {
		t.Fatalf("session get: %v", err)
	}
	var afterCount, newSentinels int
	for ev := range after.Session.Events().All() {
		afterCount++
		if beforeIDs[ev.ID] {
			continue
		}
		// Author = the appending agent's name (fakeCtx.AgentName() == "test"):
		// same-author events are the class proven to re-enter that agent's
		// request assembly (review fix - an invented author risks exclusion).
		if ev.Author != "test" || ev.Content == nil || !isSentinel(ev.Content) {
			t.Fatalf("unexpected new event: author=%q sentinel=%v", ev.Author, ev.Content != nil && isSentinel(ev.Content))
		}
		newSentinels++
	}
	if afterCount != len(beforeIDs)+1 {
		t.Fatalf("session has %d events after compaction; want %d (raw events untouched + one summary)", afterCount, len(beforeIDs)+1)
	}
	if newSentinels != 1 {
		t.Fatalf("appended %d new sentinel events; want exactly 1", newSentinels)
	}
}

// recordUsage learns the provider's FIXED overhead (system instruction + tool
// schemas) additively - the part estimateTokens structurally cannot see - rather
// than a multiplier. goose and opencode both drive compaction from the provider's
// own reported usage for exactly this reason; a fudge factor on a bytes/4 guess is
// how quack came to believe a ~7k-token request was 56,344 tokens and shredded a
// node's task.
func TestOverheadRecordedAndAppliedAdditively(t *testing.T) {
	const density = defaultCalibrationRatio

	// A turn whose content estimates at 1000 tokens but which the provider bills at
	// 7300 - the extra ~6k is the system instruction + tool schemas.
	est, measured := 1000, 7300
	overhead := measured - int(float64(est)*density)
	if overhead < 0 {
		overhead = 0
	}

	// The next turn doubles the content. The provider should bill roughly
	// overhead + density*2000 - NOT double the whole previous measurement.
	got := calibrated(2000, density, overhead)
	want := overhead + int(float64(2000)*density)
	if got != want {
		t.Fatalf("calibrated = %d, want %d (overhead must be added, not multiplied)", got, want)
	}
	// Sanity: the overhead is carried, so the estimate is well above a bare bytes/4.
	if got <= int(float64(2000)*density) {
		t.Fatalf("calibrated %d ignores the fixed overhead", got)
	}
	// And it must not explode: the old model would have multiplied by measured/est = 7.3.
	if got >= int(float64(2000)*7.3) {
		t.Fatalf("calibrated %d is as bad as the old multiplicative model", got)
	}
}
