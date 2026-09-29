package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"
)

// pageBody: a fetched page of ~2k tokens, the per-call growth seen in prod.
var pageBody = strings.Repeat("research text line\n", 420)

func artifactFor(i int) string { return fmt.Sprintf("web_page:%012d", i) }

// fetchTurn: one model web_fetch call and its (stored) result.
func fetchTurn(i int) []*genai.Content {
	id := fmt.Sprintf("call-%d", i)
	return []*genai.Content{
		{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: id, Name: toolWebFetch,
			Args: map[string]any{"urls": []any{fmt.Sprintf("https://ex.com/%d", i)}}}}}},
		{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: id, Name: toolWebFetch,
			Response: map[string]any{"results": []any{map[string]any{
				"url": fmt.Sprintf("https://ex.com/%d", i), "artifact": artifactFor(i), "lines": float64(421), "text": pageBody,
			}}}}}}},
	}
}

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

func serialize(cs []*genai.Content) string {
	var b strings.Builder
	for _, c := range cs {
		j, _ := json.Marshal(c)
		b.Write(j)
	}
	return b.String()
}

// responseText: the web_fetch entry text of contents[i]'s response.
func responseText(c *genai.Content) string {
	r := c.Parts[0].FunctionResponse.Response["results"].([]any)[0].(map[string]any)
	return r["text"].(string)
}

// TestCollapse_StaleFetchesStubbedInBatches drives the callback the way ADK
// does - a fresh request per call, rebuilt from the untouched session - over 40 fetch turns.
func TestCollapse_StaleFetchesStubbedInBatches(t *testing.T) {
	cb := collapseCallback(fakeTools(toolWebFetch, toolReadArtifact))
	sys := &genai.Content{Parts: []*genai.Part{{Text: strings.Repeat("system prompt ", 1700)}}} // ~6k tokens
	session := []*genai.Content{{Role: genai.RoleUser, Parts: []*genai.Part{{Text: "research the thing"}}}}

	const n = 40
	var prev string
	var sizes []int
	events := 0
	stubbed := 0
	var last *model.LLMRequest
	for i := 1; i <= n; i++ {
		session = append(session, fetchTurn(i)...)
		req := &model.LLMRequest{Contents: append([]*genai.Content(nil), session...), Config: &genai.GenerateContentConfig{SystemInstruction: sys}}
		if _, err := cb(nil, req); err != nil {
			t.Fatal(err)
		}
		cur := serialize(req.Contents)
		nowStubbed := strings.Count(cur, "collapsed from history")
		if nowStubbed != stubbed {
			events++
		} else if prev != "" && !strings.HasPrefix(cur, prev) {
			t.Fatalf("call %d: prompt prefix changed without a collapse batch - the vLLM prefix cache would miss every call", i)
		}
		stubbed, prev, last = nowStubbed, cur, req
		sizes = append(sizes, estimateTokens(req))
		for k := len(req.Contents) - 2*keepRecentToolTurns + 1; k < len(req.Contents); k += 2 {
			if k > 0 && responseText(req.Contents[k]) != pageBody {
				t.Fatalf("call %d: a result among the latest %d was collapsed", i, keepRecentToolTurns)
			}
		}
	}

	// Plateau: raw history reaches ~86k tokens; the sent prompt stays near the line.
	for i, s := range sizes[n/2:] {
		if s > collapseAtTokens+3_000 {
			t.Errorf("call %d: prompt ~%d tokens, want it held near %d", n/2+i+1, s, collapseAtTokens)
		}
	}
	if raw := estimateTokens(&model.LLMRequest{Contents: session, Config: &genai.GenerateContentConfig{SystemInstruction: sys}}); raw < 3*collapseAtTokens {
		t.Fatalf("test setup: raw history only ~%d tokens", raw)
	}
	t.Logf("collapse batches=%d; per-call prompt tokens: %v", events, sizes)
	if events == 0 || events > n/3 {
		t.Errorf("collapse batches = %d over %d calls, want a few batches, not one per call", events, n)
	}

	if text := responseText(last.Contents[2]); !strings.Contains(text, artifactFor(1)) {
		t.Errorf("oldest result = %q, want a stub naming %s", text, artifactFor(1))
	}
	if got := last.Contents[0].Parts[0].Text; got != "research the thing" {
		t.Errorf("user message changed to %q", got)
	}
	if responseText(session[2]) != pageBody {
		t.Error("the session's own result was mutated; only the outgoing request may change")
	}
}

// TestCollapse_ReadArtifactStubNamesItsArtifact: a read_artifact result has
// no id of its own, so the stub takes it from the matching call.
func TestCollapse_ReadArtifactStubNamesItsArtifact(t *testing.T) {
	cb := collapseCallback(fakeTools(toolReadArtifact))
	contents := []*genai.Content{
		{Role: genai.RoleModel, Parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: "r1", Name: toolReadArtifact, Args: map[string]any{"id": "web_page:abc", "offset": float64(40)}}}}},
		{Role: genai.RoleUser, Parts: []*genai.Part{{FunctionResponse: &genai.FunctionResponse{ID: "r1", Name: toolReadArtifact, Response: map[string]any{"result": strings.Repeat("window line\n", 3_000)}}}}},
	}
	for i := 1; i <= keepRecentToolTurns+5; i++ {
		contents = append(contents, fetchTurn(i)...)
	}
	req := &model.LLMRequest{Contents: contents}
	if _, err := cb(nil, req); err != nil {
		t.Fatal(err)
	}
	got, _ := req.Contents[1].Parts[0].FunctionResponse.Response["result"].(string)
	if !strings.Contains(got, "web_page:abc") || !strings.Contains(got, "collapsed") {
		t.Fatalf("read_artifact stub = %q, want it collapsed and naming web_page:abc", got)
	}
}

// TestCollapse_OffWithoutReadArtifact: a stub the agent cannot resolve would
// lose the text for good, so no read_artifact means no collapse.
func TestCollapse_OffWithoutReadArtifact(t *testing.T) {
	if collapseCallback(fakeTools(toolWebFetch)) != nil {
		t.Fatal("collapse must stay off for an agent without read_artifact")
	}
}

// growthModel calls web_fetch n times, recording each request's estimated
// size, then answers.
type growthModel struct {
	n     int
	sizes []int
}

func (m *growthModel) Name() string { return "growth" }

func (m *growthModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.sizes = append(m.sizes, estimateTokens(req))
		part := &genai.Part{Text: "done"}
		if len(m.sizes) <= m.n {
			part = &genai.Part{FunctionCall: &genai.FunctionCall{ID: fmt.Sprintf("f%d", len(m.sizes)), Name: toolWebFetch,
				Args: map[string]any{"n": len(m.sizes)}}}
		}
		yield(&model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{part}}, TurnComplete: true}, nil)
	}
}

// TestBuild_TokenGrowthPlateaus: a real llmagent built by Build, fetching 30
// pages in one round, sends a prompt that stops growing once collapse engages.
func TestBuild_TokenGrowthPlateaus(t *testing.T) {
	type fetchArgs struct {
		N int `json:"n"`
	}
	fetch, err := functiontool.New[fetchArgs, map[string]any](functiontool.Config{Name: toolWebFetch, Description: "fetch"},
		func(_ adkagent.Context, a fetchArgs) (map[string]any, error) {
			return map[string]any{"results": []any{map[string]any{"url": "https://ex.com", "artifact": artifactFor(a.N), "lines": 421, "text": pageBody}}}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	m := &growthModel{n: 30}
	b := &Bundle{Card: Card{Name: "tester", Description: "a test agent"}, Prompt: "Research."}
	ag, err := Build(b, nil, m, append([]tool.Tool{fetch}, fakeTools(toolReadArtifact)...), nil, "", nil, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{AppName: "test", Agent: ag, SessionService: session.InMemoryService(), AutoCreateSession: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range r.Run(context.Background(), "u", "s", genai.NewContentFromText("go", genai.RoleUser), adkagent.RunConfig{}) {
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("per-call prompt tokens: %v", m.sizes)
	if len(m.sizes) != m.n+1 {
		t.Fatalf("model calls = %d, want %d", len(m.sizes), m.n+1)
	}
	growth := m.sizes[1] - m.sizes[0]
	for i, s := range m.sizes {
		if s > collapseAtTokens+2*growth {
			t.Fatalf("call %d sent ~%d tokens (per-call growth ~%d): the prompt kept growing past %d; sizes=%v", i+1, s, growth, collapseAtTokens, m.sizes)
		}
	}
	if uncollapsed := m.sizes[0] + m.n*growth; uncollapsed < 2*collapseAtTokens {
		t.Fatalf("test setup: %d calls only reach ~%d tokens uncollapsed", m.n, uncollapsed)
	}
}
