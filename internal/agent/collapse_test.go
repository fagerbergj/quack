package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
	"sync"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/compaction"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"
)

// pageBody: a fetched page of ~2k tokens, the per-call growth seen in prod.
var pageBody = strings.Repeat("research text line\n", 420)

func artifactFor(i int) string { return fmt.Sprintf("web_page:%012d", i) }

func fakeTools(names ...string) []tool.Tool {
	out := make([]tool.Tool, 0, len(names))
	for _, n := range names {
		t, err := functiontool.New[map[string]any, map[string]any](functiontool.Config{Name: n, Description: n},
			func(adkagent.Context, map[string]any) (map[string]any, error) { return nil, nil })
		if err != nil {
			panic(err)
		}
		out = append(out, t)
	}
	return out
}

func ev(author string, parts ...*genai.Part) *session.Event {
	e := &session.Event{Author: author}
	e.Content = &genai.Content{Role: author, Parts: parts}
	return e
}

func call(id, name string, args map[string]any) *genai.Part {
	return &genai.Part{FunctionCall: &genai.FunctionCall{ID: id, Name: name, Args: args}}
}

func result(id, name string, resp map[string]any) *genai.Part {
	return &genai.Part{FunctionResponse: &genai.FunctionResponse{ID: id, Name: name, Response: resp}}
}

func fetched(i int) map[string]any {
	return map[string]any{"results": []any{map[string]any{"url": fmt.Sprintf("https://ex.com/%d", i), "artifact": artifactFor(i), "lines": float64(421), "text": pageBody}}}
}

// TestCollapseTranscript_StubsStoredResults: stored fetch/read results become
// artifact stubs carrying the slice they read; everything else stays verbatim.
func TestCollapseTranscript_StubsStoredResults(t *testing.T) {
	events := []*session.Event{
		ev("user", &genai.Part{Text: "research the thing"}),
		ev("model", &genai.Part{Text: "private reasoning", Thought: true}, call("c1", toolWebFetch, map[string]any{"urls": []any{"https://ex.com/1"}, "pattern": "needle"})),
		ev("user", result("c1", toolWebFetch, fetched(1))),
		ev("model", call("c2", toolReadArtifact, map[string]any{"id": "web_page:abc", "offset": float64(40), "lines": float64(30)})),
		ev("user", result("c2", toolReadArtifact, map[string]any{"result": strings.Repeat("window line\n", 300)})),
		ev("model", call("c3", "web_search", map[string]any{"queries": []any{"q"}})),
		ev("user", result("c3", "web_search", map[string]any{"queries": []any{map[string]any{"query": "q", "results": []any{}}}})),
		ev("model", &genai.Part{Text: "found it"}),
	}
	text, before, stubbed := collapseTranscript(events)
	for _, want := range []string{"User: research the thing", artifactFor(1), `pattern \"needle\"`, "web_page:abc, 301 lines read (offset 40, lines 30)", `web_search returned {"queries"`, "You: found it"} {
		if !strings.Contains(text, want) {
			t.Errorf("transcript missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "research text line") || strings.Contains(text, "window line") || strings.Contains(text, "private reasoning") {
		t.Errorf("transcript kept a stored page body or a thought:\n%.400s", text)
	}
	if stubbed != 2 || len(text)/charsPerToken >= before/4 {
		t.Errorf("stubbed=%d, ~%d tokens from ~%d; want both stored results stubbed and the window far smaller", stubbed, len(text)/charsPerToken, before)
	}
}

// TestCollapseTranscript_ReusedCallIDs: recovered calls once reused one id every
// turn; each result must still be stubbed with its own call's artifact.
func TestCollapseTranscript_ReusedCallIDs(t *testing.T) {
	var events []*session.Event
	for _, id := range []string{"web_page:first", "web_page:second"} {
		events = append(events,
			ev("model", call("rtc_0_read_artifact", toolReadArtifact, map[string]any{"id": id})),
			ev("user", result("rtc_0_read_artifact", toolReadArtifact, map[string]any{"result": strings.Repeat(id+" text\n", 200)})))
	}
	text, _, _ := collapseTranscript(events)
	first, second := strings.Index(text, "[web_page:first,"), strings.Index(text, "[web_page:second,")
	if first < 0 || second < first {
		t.Fatalf("stubs must pair with their own call, in order:\n%s", text)
	}
}

// countingSummarizer is the model summarizer a collapse should make unnecessary.
type countingSummarizer struct{ calls int }

func (c *countingSummarizer) SummarizeEvents(context.Context, []*session.Event) (compaction.SummarizeResult, error) {
	c.calls++
	return compaction.SummarizeResult{Content: genai.NewContentFromText("MODEL SUMMARY", genai.RoleModel)}, nil
}

// TestCollapsingSummarizer_FallsBackWhenStubsDoNotFit: only a collapse that
// brings the prompt back under the threshold replaces the model summary.
func TestCollapsingSummarizer_FallsBackWhenStubsDoNotFit(t *testing.T) {
	window := []*session.Event{
		ev("model", call("c1", toolWebFetch, map[string]any{"urls": []any{"u"}})),
		ev("user", result("c1", toolWebFetch, fetched(1))),
	}
	// lastEst 0: a reused/resumed node's first compaction, before any measurement.
	for _, c := range []struct {
		prompt, lastEst int
		wantModel       bool
	}{{10_000, 10_000, false}, {40_000, 40_000, true}, {10_000, 0, true}} {
		inner := &countingSummarizer{}
		s := collapsingSummarizer{inner: inner, meter: &PromptMeter{tokens: c.prompt, lastEst: c.lastEst}, threshold: 9_000}
		got, err := s.SummarizeEvents(context.Background(), window)
		if err != nil {
			t.Fatal(err)
		}
		if (inner.calls == 1) != c.wantModel || !strings.Contains(got.Content.Parts[0].Text, map[bool]string{true: "MODEL SUMMARY", false: artifactFor(1)}[c.wantModel]) {
			t.Errorf("prompt ~%d tokens vs threshold 9000: summarizer calls=%d, content %.80q", c.prompt, inner.calls, got.Content.Parts[0].Text)
		}
	}
}

// TestPromptMeter_SkipsThoughtsAndCalibrates: thoughts are dropped before sending, and the
// observed count adds the overhead the estimate misses (tool schemas), which a collapse keeps.
func TestPromptMeter_SkipsThoughtsAndCalibrates(t *testing.T) {
	m := NewPromptMeter()
	req := &model.LLMRequest{Contents: []*genai.Content{
		{Role: genai.RoleUser, Parts: []*genai.Part{{Text: strings.Repeat("x", 4_000)}}},
		{Role: genai.RoleModel, Parts: []*genai.Part{{Text: strings.Repeat("t", 400_000), Thought: true}}},
	}}
	_, _ = m.beforeModel(nil, req)
	if m.tokens != 1_000 {
		t.Fatalf("estimate = %d, want 1000 (thought parts are never sent)", m.tokens)
	}
	_, _ = m.afterModel(nil, &model.LLMResponse{UsageMetadata: &genai.GenerateContentResponseUsageMetadata{PromptTokenCount: 2_000}}, nil)
	// Observed 2000 = estimate 1000 + 1000 overhead; saving 200 leaves exactly 1800, 90% of 2000.
	if !m.fitsAfter(200, 2_000) || m.fitsAfter(199, 2_000) {
		t.Errorf("offset = %d: a collapse leaving exactly 90%% of the threshold fits, one token more does not", m.offset)
	}
}

// TestPromptMeter_DeclinesBeforeFirstCall: compaction runs ahead of a build's first
// model call (node reuse, resume); knowing nothing, the meter must not accept a collapse.
func TestPromptMeter_DeclinesBeforeFirstCall(t *testing.T) {
	if NewPromptMeter().fitsAfter(85_000, 50_000) {
		t.Fatal("an uncalibrated meter accepted a collapse; ADK would then block compaction for the whole turn")
	}
}

// growthModel calls web_fetch n times, recording each request's estimated size, then answers.
type growthModel struct {
	mu    sync.Mutex
	n     int
	sizes []int
	sent  []string // each request's contents, serialized, to check the prefix between compactions
}

func (m *growthModel) Name() string { return "growth" }

func (m *growthModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.mu.Lock()
		m.sizes = append(m.sizes, estimateTokens(req))
		j, _ := json.Marshal(req.Contents)
		m.sent = append(m.sent, strings.TrimSuffix(string(j), "]"))
		k := len(m.sizes)
		m.mu.Unlock()
		part := &genai.Part{Text: "done"}
		if k <= m.n {
			part = call(fmt.Sprintf("f%d", k), toolWebFetch, map[string]any{"n": k})
		}
		yield(&model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{part}}, TurnComplete: true}, nil)
	}
}

// summaryModel stands in for the compaction model, counting its calls in *calls.
func summaryModel(calls *int) *fakeLLM {
	return &fakeLLM{func(*model.LLMRequest) *model.LLMResponse {
		*calls++
		return &model.LLMResponse{Content: genai.NewContentFromText("MODEL SUMMARY", genai.RoleModel), TurnComplete: true}
	}}
}

// runGrowth runs one 30-fetch round under tail-retention compaction at threshold,
// returning the per-call prompt sizes and how often the compaction model ran.
func runGrowth(t *testing.T, threshold int, stored bool) ([]int, []string, int) {
	t.Helper()
	type fetchArgs struct {
		N int `json:"n"`
	}
	fetch, err := functiontool.New[fetchArgs, map[string]any](functiontool.Config{Name: toolWebFetch, Description: "fetch"},
		func(_ adkagent.Context, a fetchArgs) (map[string]any, error) {
			r := fetched(a.N)
			if !stored {
				delete(r["results"].([]any)[0].(map[string]any), "artifact")
			}
			return r, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	var summCalls int
	m, summ, meter := &growthModel{n: 30}, summaryModel(&summCalls), NewPromptMeter()
	b := &Bundle{Card: Card{Name: "tester", Description: "a test agent"}, Prompt: "Research."}
	ag, err := Build(b, nil, m, append([]tool.Tool{fetch}, fakeTools(toolReadArtifact)...), nil, "", "", nil, meter)
	if err != nil {
		t.Fatal(err)
	}
	comp, err := NativeCompactionConfig(Compaction{Enabled: true, Summarizer: summ, TokenThreshold: threshold, EventRetentionSize: 6, Meter: meter})
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{AppName: "test", Agent: ag, SessionService: session.InMemoryService(), AutoCreateSession: true, Compaction: comp})
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range r.Run(context.Background(), "u", "s", genai.NewContentFromText("go", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil && !strings.Contains(err.Error(), "compaction") {
			t.Fatal(err)
		}
	}
	return m.sizes, m.sent, summCalls
}

// TestCompaction_CollapsesBeforeSummarizing: the prompt grows to the compaction
// threshold, then drops via stubs with no summarizer call, and stays bounded.
func TestCompaction_CollapsesBeforeSummarizing(t *testing.T) {
	const threshold = 16_000
	sizes, sent, summaries := runGrowth(t, threshold, true)
	t.Logf("per-call prompt tokens: %v", sizes)
	if summaries != 0 {
		t.Errorf("compaction model ran %d times; stubs alone should bring the prompt back under", summaries)
	}
	drops := 0
	for i := 1; i < len(sizes); i++ {
		if sizes[i] > threshold+3_000 {
			t.Fatalf("call %d sent ~%d tokens, past threshold %d: %v", i+1, sizes[i], threshold, sizes)
		}
		if sizes[i] < sizes[i-1] {
			drops++
			if sizes[i-1] < threshold*8/10 {
				t.Errorf("call %d: prompt dropped at ~%d tokens, before reaching the threshold - a collapse off the compaction trigger", i+1, sizes[i-1])
			}
		} else if !strings.HasPrefix(sent[i], sent[i-1]) {
			t.Errorf("call %d: the prompt prefix changed without a compaction - the prefix cache would miss", i+1)
		}
	}
	if drops < 3 || sizes[len(sizes)-1] >= 30*2_000 {
		t.Fatalf("prompt dropped %d times, want several compactions: %v", drops, sizes)
	}
	// Each compaction rolls up the last; the first page's stub must survive all of them.
	if !strings.Contains(sent[len(sent)-1], artifactFor(1)) {
		t.Error("after several compactions the last request no longer names the first fetched page's artifact")
	}
}

// TestCompaction_SummarizesWhenNothingStored: with no artifacts to point at,
// compaction falls through to the model summarizer as before.
func TestCompaction_SummarizesWhenNothingStored(t *testing.T) {
	_, _, summaries := runGrowth(t, 16_000, false)
	if summaries == 0 {
		t.Fatal("unstored results cannot be stubbed, so the compaction model must summarize")
	}
}

// TestCompaction_OffWithoutReadArtifactOrCompaction: without read_artifact a stub
// could never be followed back; without compaction nothing collapses at all.
func TestCompaction_OffWithoutReadArtifactOrCompaction(t *testing.T) {
	meter := NewPromptMeter()
	if _, err := Build(&Bundle{Card: Card{Name: "t"}}, nil, &growthModel{}, fakeTools(toolWebFetch), nil, "", "", nil, meter); err != nil {
		t.Fatal(err)
	}
	comp, err := NativeCompactionConfig(Compaction{Enabled: true, Summarizer: summaryModel(new(int)), TokenThreshold: 1_000, EventRetentionSize: 2, Meter: meter})
	if err != nil {
		t.Fatal(err)
	}
	if _, wrapped := comp.Summarizer.(collapsingSummarizer); wrapped {
		t.Error("an agent without read_artifact must never get the collapsing summarizer")
	}
	if off, _ := NativeCompactionConfig(Compaction{Meter: meter}); off != nil {
		t.Error("compaction disabled must mean no compaction and so no collapse")
	}
}

// TestCollapseTranscript_CarriesEarlierSummary: a rolled-up earlier summary is
// carried forward once, unlabelled, not re-attributed to the user.
func TestCollapseTranscript_CarriesEarlierSummary(t *testing.T) {
	// ADK's snapshot shape: the earlier summary rides in Content, CompactedContent is nil.
	prior := ev("model", &genai.Part{Text: collapseHeader + "You: earlier finding\n"})
	prior.Actions.Compaction = &session.EventCompaction{}
	text, _, _ := collapseTranscript([]*session.Event{prior, ev("model", &genai.Part{Text: "next"})})
	if strings.Count(text, collapseHeader) != 1 || !strings.Contains(text, "\nYou: earlier finding\nYou: next\n") || strings.Contains(text, "User:") {
		t.Fatalf("transcript = %q, want the earlier summary once, unlabelled", text)
	}
}

// TestCollapseTranscript_AttachmentPlaceholder: media is named, never inlined or dropped silently.
func TestCollapseTranscript_AttachmentPlaceholder(t *testing.T) {
	text, _, _ := collapseTranscript([]*session.Event{
		ev("user", &genai.Part{InlineData: &genai.Blob{MIMEType: "image/png", Data: []byte("png")}}),
		ev("model", &genai.Part{ExecutableCode: &genai.ExecutableCode{Code: "print(1)"}}, &genai.Part{CodeExecutionResult: &genai.CodeExecutionResult{Output: "1"}}),
	})
	for _, want := range []string{"User: [image/png attachment]", "You: [executable code]", "You: [code execution result]"} {
		if !strings.Contains(text, want) {
			t.Errorf("transcript = %q, want placeholder %q", text, want)
		}
	}
}
