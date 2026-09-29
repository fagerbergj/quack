package agent

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

const (
	// collapseAtTokens: estimated prompt size past which a batch of stale tool
	// results is collapsed; it sits far below ADK compaction's window-20k trigger.
	collapseAtTokens = 24_000
	// keepRecentToolTurns: tool-result turns always sent whole, so the model
	// still sees what it just read.
	keepRecentToolTurns = 3
	// minCollapseChars: a smaller result costs about what its stub does.
	minCollapseChars = 1_000

	toolWebFetch     = "web_fetch"
	toolReadArtifact = "read_artifact"
)

// historyCollapser replaces stale web_fetch/read_artifact results in each
// request with a stub naming the artifact that still holds the text. The session keeps the full results; only what is re-sent shrinks.
type historyCollapser struct {
	mu sync.Mutex
	// collapsed holds FunctionResponse ids stubbed so far. It only grows, so
	// every request between two batches shares a byte-identical prefix (the vLLM prefix cache breaks once per batch, not per call).
	collapsed map[string]bool
}

// collapseCallback returns nil for an agent without read_artifact: its stubs
// would name an artifact it has no way to read.
func collapseCallback(tools []tool.Tool) llmagent.BeforeModelCallback {
	if !slices.ContainsFunc(tools, func(t tool.Tool) bool { return t.Name() == toolReadArtifact }) {
		return nil
	}
	c := &historyCollapser{collapsed: map[string]bool{}}
	return c.before
}

func (c *historyCollapser) before(_ adkagent.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
	if req == nil {
		return nil, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	readIDs := readArtifactIDs(req.Contents)
	c.apply(req, readIDs)
	// Collapse everything stale at once when over the line: one prefix-cache
	// miss buys many calls of headroom, where a sliding window would miss every call.
	if estimateTokens(req) > collapseAtTokens && c.markStale(req.Contents, readIDs) {
		c.apply(req, readIDs)
	}
	return nil, nil
}

// markStale adds every collapsible result older than the last
// keepRecentToolTurns tool-result turns; false when nothing new qualified.
func (c *historyCollapser) markStale(contents []*genai.Content, readIDs map[string]string) bool {
	var turns []int
	for i, ct := range contents {
		if ct != nil && slices.ContainsFunc(ct.Parts, func(p *genai.Part) bool { return p != nil && p.FunctionResponse != nil }) {
			turns = append(turns, i)
		}
	}
	if len(turns) <= keepRecentToolTurns {
		return false
	}
	added := false
	for _, i := range turns[:len(turns)-keepRecentToolTurns] {
		for _, p := range contents[i].Parts {
			if p == nil || p.FunctionResponse == nil || c.collapsed[p.FunctionResponse.ID] {
				continue
			}
			if _, ok := stubFor(p.FunctionResponse, readIDs); ok {
				c.collapsed[p.FunctionResponse.ID] = true
				added = true
			}
		}
	}
	return added
}

// apply swaps each collapsed result for its stub in copies, never touching
// the session's own Content values.
func (c *historyCollapser) apply(req *model.LLMRequest, readIDs map[string]string) {
	if len(c.collapsed) == 0 {
		return
	}
	for i, ct := range req.Contents {
		if ct == nil {
			continue
		}
		var parts []*genai.Part
		for j, p := range ct.Parts {
			if p == nil || p.FunctionResponse == nil || !c.collapsed[p.FunctionResponse.ID] {
				continue
			}
			stub, ok := stubFor(p.FunctionResponse, readIDs)
			if !ok {
				continue
			}
			if parts == nil {
				parts = slices.Clone(ct.Parts)
			}
			fr := *p.FunctionResponse
			fr.Response = stub
			parts[j] = &genai.Part{FunctionResponse: &fr}
		}
		if parts != nil {
			req.Contents[i] = &genai.Content{Role: ct.Role, Parts: parts}
		}
	}
}

// stubFor builds fr's collapsed response; false when fr is not a large enough
// web_fetch/read_artifact result whose text a stored artifact still holds.
func stubFor(fr *genai.FunctionResponse, readIDs map[string]string) (map[string]any, bool) {
	if fr.ID == "" || fr.Response["error"] != nil || responseChars(fr.Response) < minCollapseChars {
		return nil, false
	}
	switch fr.Name {
	case toolWebFetch:
		return fetchStub(fr.Response)
	case toolReadArtifact:
		id := readIDs[fr.ID]
		text, _ := fr.Response["result"].(string)
		if id == "" || text == "" {
			return nil, false
		}
		return map[string]any{"result": fmt.Sprintf("[%s, %d lines read - collapsed from history; read_artifact(id, offset, lines) re-reads it]",
			id, strings.Count(text, "\n")+1)}, true
	}
	return nil, false
}

// fetchStub keeps each entry's url/artifact/lines and drops its text; false
// when a successful entry has no artifact to point at.
func fetchStub(resp map[string]any) (map[string]any, bool) {
	entries, ok := resp["results"].([]any)
	if !ok {
		return nil, false
	}
	out := make([]any, 0, len(entries))
	for _, e := range entries {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, false
		}
		if m["error"] != nil {
			out = append(out, m)
			continue
		}
		id, _ := m["artifact"].(string)
		if id == "" {
			return nil, false
		}
		out = append(out, map[string]any{"url": m["url"], "artifact": id, "lines": m["lines"],
			"text": fmt.Sprintf("[%s, %v lines - collapsed from history; read_artifact(id, offset, lines) or grep_artifacts(pattern, ids) re-reads it]", id, m["lines"])})
	}
	return map[string]any{"results": out}, true
}

// readArtifactIDs maps each read_artifact call id to the artifact it read.
func readArtifactIDs(contents []*genai.Content) map[string]string {
	out := map[string]string{}
	for _, ct := range contents {
		if ct == nil {
			continue
		}
		for _, p := range ct.Parts {
			if p == nil || p.FunctionCall == nil || p.FunctionCall.Name != toolReadArtifact {
				continue
			}
			if id, _ := p.FunctionCall.Args["id"].(string); id != "" {
				out[p.FunctionCall.ID] = id
			}
		}
	}
	return out
}

// estimateTokens approximates req's prompt size at charsPerToken.
func estimateTokens(req *model.LLMRequest) int {
	n := 0
	if req.Config != nil && req.Config.SystemInstruction != nil {
		n += contentChars(req.Config.SystemInstruction)
	}
	for _, ct := range req.Contents {
		n += contentChars(ct)
	}
	return n / charsPerToken
}

func contentChars(ct *genai.Content) int {
	if ct == nil {
		return 0
	}
	n := 0
	for _, p := range ct.Parts {
		switch {
		case p == nil:
		case p.FunctionResponse != nil:
			n += responseChars(p.FunctionResponse.Response)
		case p.FunctionCall != nil:
			n += responseChars(p.FunctionCall.Args)
		default:
			n += len(p.Text)
		}
	}
	return n
}

func responseChars(m map[string]any) int {
	b, _ := json.Marshal(m)
	return len(b)
}
