package acp

import (
	"context"
	"iter"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/vetting"
)

// fakeMCPEmbedder returns a fixed unit vector for every text, so a recall
// query matches any stored point (cosine = 1) — the round-trip below is
// exercised through the SCOPE filter, not embedding similarity.
type fakeMCPEmbedder struct{}

func (fakeMCPEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0, 0, 0}
	}
	return out, nil
}

// verbatimConsolidator replies with a single ADD op that writes sourceText
// verbatim — just enough to seed a bucket through the real Store.Commit path.
type verbatimConsolidator struct{}

func (verbatimConsolidator) Name() string { return "verbatim-consolidator" }

func (verbatimConsolidator) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		var text string
		for _, c := range req.Contents {
			for _, p := range c.Parts {
				if p != nil && p.Text != "" {
					text = p.Text
				}
			}
		}
		start := strings.Index(text, "FINAL ANSWER (extract additional durable tradecraft from it):\n")
		content := "seed"
		if start >= 0 {
			rest := text[start+len("FINAL ANSWER (extract additional durable tradecraft from it):\n"):]
			if end := strings.IndexByte(rest, '\n'); end >= 0 {
				content = rest[:end]
			}
		}
		reply := `{"ops":[{"action":"ADD","content":"` + content + `","kind":"note"}]}`
		yield(&model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: reply}}}}, nil)
	}
}

// TestMemoryMCP_LoadMemory_ScopedRecall proves load_memory resolves ONLY the
// buckets the registered node is entitled to — never a bucket named by the tool
// call itself, satisfying #344's "scope from registration, not args" rule.
func TestMemoryMCP_LoadMemory_ScopedRecall(t *testing.T) {
	ctx := context.Background()
	store, err := memory.OpenSQLite(ctx, t.TempDir()+"/mem.db", fakeMCPEmbedder{}, verbatimConsolidator{}, "test_mcp_load", "task", 5, 0)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	inScope := memory.Scope{Repo: "acme/in-scope"}
	outOfScope := memory.Scope{Repo: "acme/out-of-scope"}
	if _, err := store.Commit(ctx, inScope, "seed", nil, "run go test ./... before every commit"); err != nil {
		t.Fatalf("seed in-scope: %v", err)
	}
	if _, err := store.Commit(ctx, outOfScope, "seed", nil, "the other repo uses npm test"); err != nil {
		t.Fatalf("seed out-of-scope: %v", err)
	}

	token := "plan1/load-node"
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{
		NodeID: "load-node", Memory: store, MemoryScope: inScope,
	})
	defer vetting.UnregisterAdvisorThread(token)

	ts := httptest.NewServer(memoryMCPHandler())
	defer ts.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.0"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/" + token}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cs.Close()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "load_memory", Arguments: map[string]any{"query": "test commands"}})
	if err != nil {
		t.Fatalf("CallTool load_memory: %v", err)
	}
	text := toolResultText(t, res)
	if !strings.Contains(text, "go test") {
		t.Fatalf("load_memory result missing the in-scope memory: %q", text)
	}
	if strings.Contains(text, "npm test") {
		t.Fatalf("load_memory leaked an out-of-scope memory: %q", text)
	}
}

// TestMemoryMCP_StageMemory_LandsInBuffer proves stage_memory writes into the
// NODE's own MemStage — the buffer the gate drains into commitMemoryOnPass —
// resolved from the URL token, never a tool argument.
func TestMemoryMCP_StageMemory_LandsInBuffer(t *testing.T) {
	ctx := context.Background()
	store, err := memory.OpenSQLite(ctx, t.TempDir()+"/mem.db", fakeMCPEmbedder{}, verbatimConsolidator{}, "test_mcp_stage", "task", 5, 0)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}

	token := "plan1/stage-node"
	stage := &vetting.MemStage{}
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{NodeID: "stage-node", Memory: store, Staged: stage})
	defer vetting.UnregisterAdvisorThread(token)

	ts := httptest.NewServer(memoryMCPHandler())
	defer ts.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0.0.0"}, nil)
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/" + token}, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer cs.Close()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{
		Name:      "stage_memory",
		Arguments: map[string]any{"content": "the build runs via make build", "kind": "repo"},
	})
	if err != nil {
		t.Fatalf("CallTool stage_memory: %v", err)
	}
	if res.IsError {
		t.Fatalf("stage_memory returned an error: %s", toolResultText(t, res))
	}

	staged := stage.Drain()
	if len(staged) != 1 || staged[0].Content != "the build runs via make build" {
		t.Fatalf("staged = %+v, want exactly one candidate with the tool's content", staged)
	}
	if staged[0].Metadata["bucket"] != "repo" {
		t.Fatalf("staged bucket = %q, want %q", staged[0].Metadata["bucket"], "repo")
	}
}

func toolResultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}
