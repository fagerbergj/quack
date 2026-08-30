package agent

import (
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// --- interval trigger --------------------------------------------------

// TestIntervalTriggerFiresOnCadence exercises the ADK compaction_interval
// cadence: under a huge ContextWindow (threshold never trips) compaction
// must still fire once invocation count hits a multiple of the interval.
func TestIntervalTriggerFiresOnCadence(t *testing.T) {
	llm := &fakeLLM{text: "## Goal\n- compacted"}
	comp := Compaction{Summarizer: llm, ContextWindow: 1_000_000, Enabled: true, CompactionInterval: 3, EventRetentionSize: 2}
	cb := compactionCallback(comp)

	contents := []*genai.Content{textContent(genai.RoleUser, "task")}
	for i := 0; i < 10; i++ {
		contents = append(contents, toolCall("read_file", "c"))
		contents = append(contents, toolResult("read_file", "c", 100))
	}
	req := func() *model.LLMRequest { return &model.LLMRequest{Contents: append([]*genai.Content{}, contents...)} }

	ctx := newFakeCtx()
	for i, invID := range []string{"inv-1", "inv-2", "inv-3"} {
		ctx.invID = invID
		if _, err := cb(ctx, req()); err != nil {
			t.Fatalf("invocation %d: callback err: %v", i, err)
		}
	}
	if llm.calls != 1 {
		t.Fatalf("interval=3 over 3 invocations: summariser calls=%d, want 1 (fires on the 3rd)", llm.calls)
	}
}

// TestIntervalTriggerFiresOncePerInvocation guards against a chatty agent
// loop (several model calls within ONE invocation) re-triggering the cadence
// every call.
func TestIntervalTriggerFiresOncePerInvocation(t *testing.T) {
	llm := &fakeLLM{text: "## Goal\n- compacted"}
	comp := Compaction{Summarizer: llm, ContextWindow: 1_000_000, Enabled: true, CompactionInterval: 1, EventRetentionSize: 2}
	cb := compactionCallback(comp)

	contents := []*genai.Content{textContent(genai.RoleUser, "task")}
	for i := 0; i < 10; i++ {
		contents = append(contents, toolCall("read_file", "c"))
		contents = append(contents, toolResult("read_file", "c", 100))
	}
	req := func() *model.LLMRequest { return &model.LLMRequest{Contents: append([]*genai.Content{}, contents...)} }

	ctx := newFakeCtx()
	ctx.invID = "inv-1"
	for i := 0; i < 4; i++ {
		if _, err := cb(ctx, req()); err != nil {
			t.Fatalf("call %d: callback err: %v", i, err)
		}
	}
	if llm.calls != 1 {
		t.Fatalf("4 model calls within one invocation: summariser calls=%d, want 1", llm.calls)
	}
}

// TestIntervalDisabledByDefault confirms CompactionInterval: 0 (unset config)
// keeps the pre-ADK-port behaviour: threshold is the only trigger.
func TestIntervalDisabledByDefault(t *testing.T) {
	llm := &fakeLLM{text: "S"}
	comp := Compaction{Summarizer: llm, ContextWindow: 1_000_000, Enabled: true}
	if intervalTrigger(newFakeCtx(), comp) {
		t.Fatal("interval trigger fired with CompactionInterval unset")
	}
	if llm.calls != 0 {
		t.Fatalf("summariser called: calls=%d", llm.calls)
	}
}

// --- overlap -------------------------------------------------------------

// TestOverlapCarriesRawEventsForward checks OverlapSize keeps the tail of a
// compacted window as raw, visible content (not folded into the sentinel
// text), so it survives to be re-offered to the summariser next round.
func TestOverlapCarriesRawEventsForward(t *testing.T) {
	llm := &fakeLLM{text: "## Goal\n- compacted"}
	comp := Compaction{Summarizer: llm, ContextWindow: 1_000_000, Enabled: true, EventRetentionSize: 2, OverlapSize: 2}
	cb := compactionCallback(comp)

	contents := []*genai.Content{textContent(genai.RoleUser, "task")}
	for i := 0; i < 10; i++ {
		contents = append(contents, toolCall("read_file", "c"))
		contents = append(contents, toolResult("read_file", "c", 100))
	}
	req := &model.LLMRequest{Contents: contents}

	ctx := newFakeCtx()
	// Force a compaction round regardless of threshold via the interval trigger.
	comp.CompactionInterval = 1
	cb = compactionCallback(comp)
	if _, err := cb(ctx, req); err != nil {
		t.Fatalf("callback err: %v", err)
	}
	idx := sentinelIndex(req.Contents)
	if idx < 0 {
		t.Fatal("no sentinel appended")
	}
	// [task, sentinel, ...2 overlap events, ...2 retention tail events]
	if got, want := len(req.Contents), 1+1+2+2; got != want {
		t.Fatalf("contents length = %d, want %d ([task, sentinel, overlap, tail])", got, want)
	}
}

// TestOverlapZeroFoldsEverything checks OverlapSize: 0 (unset) leaves no raw
// remainder - full backward-compatible behaviour.
func TestOverlapZeroFoldsEverything(t *testing.T) {
	llm := &fakeLLM{text: "## Goal\n- compacted"}
	comp := Compaction{Summarizer: llm, ContextWindow: 1_000_000, Enabled: true, EventRetentionSize: 2, CompactionInterval: 1}
	cb := compactionCallback(comp)

	contents := []*genai.Content{textContent(genai.RoleUser, "task")}
	for i := 0; i < 10; i++ {
		contents = append(contents, toolCall("read_file", "c"))
		contents = append(contents, toolResult("read_file", "c", 100))
	}
	req := &model.LLMRequest{Contents: contents}
	if _, err := cb(newFakeCtx(), req); err != nil {
		t.Fatalf("callback err: %v", err)
	}
	if got, want := len(req.Contents), 1+1+2; got != want {
		t.Fatalf("contents length = %d, want %d ([task, sentinel, tail])", got, want)
	}
}

// --- oversized window chunking -------------------------------------------

// TestOversizedWindowChunksIteratively: a head far bigger than the
// summariser's own input budget must be summarised in several chunk calls
// (running summary + next chunk), never truncated.
func TestOversizedWindowChunksIteratively(t *testing.T) {
	llm := &fakeLLM{text: "## Goal\n- chunk summary"}
	// ContextWindow small enough that usable() leaves a tight chunk budget.
	comp := Compaction{Summarizer: llm, ContextWindow: 25_000, Enabled: true, EventRetentionSize: 2, CompactionInterval: 1}
	cb := compactionCallback(comp)

	contents := []*genai.Content{textContent(genai.RoleUser, "task")}
	for i := 0; i < 20; i++ {
		contents = append(contents, toolCall("read_file", "c"))
		contents = append(contents, toolResult("read_file", "c", 4_000))
	}
	req := &model.LLMRequest{Contents: contents}
	if _, err := cb(newFakeCtx(), req); err != nil {
		t.Fatalf("callback err: %v", err)
	}
	if llm.calls < 2 {
		t.Fatalf("oversized window summarised in %d call(s), want >= 2 (chunked)", llm.calls)
	}
	if sentinelIndex(req.Contents) < 0 {
		t.Fatal("no sentinel appended despite chunked summarisation")
	}
}

// TestChunkByBudgetRespectsToolCallBoundaries: chunk cuts must land on a
// balanced FunctionCall/FunctionResponse boundary, never splitting a call
// from its own response into different chunks.
func TestChunkByBudgetRespectsToolCallBoundaries(t *testing.T) {
	var head []*genai.Content
	for i := 0; i < 6; i++ {
		head = append(head, toolCall("read_file", "c"))
		head = append(head, toolResult("read_file", "c", 500))
	}
	// Budget forces a cut mid-way through the sequence.
	chunks := chunkByBudget(head, 1200)
	if len(chunks) < 2 {
		t.Fatalf("expected chunking with a tight budget, got %d chunk(s)", len(chunks))
	}
	for ci, chunk := range chunks {
		open := map[string]bool{}
		for _, c := range chunk {
			for _, p := range c.Parts {
				if p.FunctionCall != nil {
					open[p.FunctionCall.ID] = true
				}
				if p.FunctionResponse != nil {
					delete(open, p.FunctionResponse.ID)
				}
			}
		}
		if len(open) != 0 {
			t.Fatalf("chunk %d ends with an unbalanced tool call (response split into a later chunk)", ci)
		}
	}
}

// TestChunkByBudgetNoTruncation: every event from the input must appear in
// exactly one output chunk - no hard truncation of the head, ever.
func TestChunkByBudgetNoTruncation(t *testing.T) {
	var head []*genai.Content
	for i := 0; i < 8; i++ {
		head = append(head, toolCall("read_file", "c"))
		head = append(head, toolResult("read_file", "c", 3_000))
	}
	chunks := chunkByBudget(head, 2_000)
	total := 0
	for _, c := range chunks {
		total += len(c)
	}
	if total != len(head) {
		t.Fatalf("chunking dropped events: got %d total across chunks, want %d", total, len(head))
	}
}
