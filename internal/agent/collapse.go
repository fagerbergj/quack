package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/session/compaction"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

const (
	// minCollapseChars: a smaller result costs about what its stub does.
	minCollapseChars = 1_000
	// collapseFitPercent: a collapse must leave the prompt this far under the
	// threshold, so the next call does not compact again straight away.
	collapseFitPercent = 90

	toolWebFetch     = "web_fetch"
	toolReadArtifact = "read_artifact"

	collapseHeader = "[Earlier in this task, verbatim except that stored web_fetch/read_artifact results are replaced by " +
		"their artifact ids; read_artifact(id, offset, lines) or grep_artifacts(pattern, ids) re-reads any of them.]\n\n"
)

// PromptMeter tracks a worker's current prompt size between model calls, so
// a compaction can tell whether collapsing stale tool results alone brings it back under the threshold.
type PromptMeter struct {
	mu         sync.Mutex
	tokens     int     // estimated prompt tokens now, including results since the last call
	lastEst    int     // estimate of the last request, to calibrate against its observed count
	scale      float64 // observed/estimated prompt tokens from the last response; 0 = uncalibrated
	resolvable bool    // the agent has read_artifact, so a stub can be followed back
}

// NewPromptMeter returns a meter for one worker; Build wires its callbacks.
func NewPromptMeter() *PromptMeter { return &PromptMeter{} }

func (m *PromptMeter) beforeModel(_ adkagent.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
	if req != nil {
		m.mu.Lock()
		m.lastEst = estimateTokens(req)
		m.tokens = m.lastEst
		m.mu.Unlock()
	}
	return nil, nil
}

func (m *PromptMeter) afterModel(_ adkagent.Context, resp *model.LLMResponse, _ error) (*model.LLMResponse, error) {
	if resp == nil || resp.Partial {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if u := resp.UsageMetadata; u != nil && u.PromptTokenCount > 0 && m.lastEst > 0 {
		m.scale = float64(u.PromptTokenCount) / float64(m.lastEst)
	}
	m.tokens += contentChars(resp.Content) / charsPerToken
	return nil, nil
}

func (m *PromptMeter) afterTool(_ adkagent.Context, _ tool.Tool, _, result map[string]any, _ error) (map[string]any, error) {
	m.mu.Lock()
	m.tokens += responseChars(result) / charsPerToken
	m.mu.Unlock()
	return nil, nil
}

// wire installs the meter on cfg; it only measures, never changing a request or result.
func (m *PromptMeter) wire(cfg *llmagent.Config) {
	m.resolvable = slices.ContainsFunc(cfg.Tools, func(t tool.Tool) bool { return t.Name() == toolReadArtifact })
	cfg.BeforeModelCallbacks = append(cfg.BeforeModelCallbacks, m.beforeModel)
	cfg.AfterModelCallbacks = append(cfg.AfterModelCallbacks, m.afterModel)
	cfg.AfterToolCallbacks = append(cfg.AfterToolCallbacks, m.afterTool)
}

// fitsAfter reports whether saving `saved` estimated tokens leaves the prompt
// under collapseFitPercent of threshold, in the model's own token units when calibrated.
func (m *PromptMeter) fitsAfter(saved, threshold int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	scale := m.scale
	if scale <= 0 {
		scale = 1
	}
	return float64(m.tokens-saved)*scale*100 <= float64(threshold*collapseFitPercent)
}

// collapsingSummarizer runs at compaction: it first replaces the window with a
// verbatim transcript whose stored fetch/read results are artifact stubs, and calls the model summarizer only when that would not fit.
type collapsingSummarizer struct {
	inner     compaction.Summarizer
	meter     *PromptMeter
	threshold int
}

func (s collapsingSummarizer) SummarizeEvents(ctx context.Context, events []*session.Event) (compaction.SummarizeResult, error) {
	text, before, stubbed := collapseTranscript(events)
	if stubbed > 0 && s.meter.fitsAfter(before-len(text)/charsPerToken, s.threshold) {
		return compaction.SummarizeResult{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: text}}}}, nil
	}
	return s.inner.SummarizeEvents(ctx, events)
}

// collapseTranscript renders events as prose (a summary may only hold text),
// stubbing each stored web_fetch/read_artifact result; before is the window's estimated tokens as sent.
func collapseTranscript(events []*session.Event) (text string, before, stubbed int) {
	var b strings.Builder
	b.WriteString(collapseHeader)
	args := map[string]map[string]any{}
	chars := 0
	for _, ev := range events {
		if ev == nil {
			continue
		}
		content, author := ev.Content, ev.Author
		if c := ev.Actions.Compaction; c != nil {
			content, author = c.CompactedContent, "" // an earlier summary: carried forward as-is
		}
		if content == nil {
			continue
		}
		chars += contentChars(content)
		for _, p := range content.Parts {
			stubbed += renderPart(&b, author, p, args)
		}
	}
	return b.String(), chars / charsPerToken, stubbed
}

// renderPart writes one part; it returns 1 when it wrote a stub. A response pairs with
// the latest call of its id, so a reused id still gets its own call's args.
func renderPart(b *strings.Builder, author string, p *genai.Part, args map[string]map[string]any) int {
	switch {
	case p == nil || p.Thought:
	case p.FunctionCall != nil:
		args[p.FunctionCall.ID] = p.FunctionCall.Args
		fmt.Fprintf(b, "You called %s(%s)\n", p.FunctionCall.Name, marshal(p.FunctionCall.Args))
	case p.FunctionResponse != nil:
		fr := p.FunctionResponse
		if stub, ok := stubFor(fr, args[fr.ID]); ok {
			fmt.Fprintf(b, "%s returned %s\n", fr.Name, marshal(stub))
			return 1
		}
		fmt.Fprintf(b, "%s returned %s\n", fr.Name, marshal(fr.Response))
	case p.Text != "" && author == "":
		fmt.Fprintf(b, "%s\n", strings.TrimPrefix(p.Text, collapseHeader))
	case p.Text != "" && author == "user":
		fmt.Fprintf(b, "User: %s\n", p.Text)
	case p.Text != "":
		fmt.Fprintf(b, "You: %s\n", p.Text)
	}
	return 0
}

func marshal(m map[string]any) string {
	out, _ := json.Marshal(m)
	return string(out)
}

// stubFor builds fr's collapsed response from its call's args; false when fr is not
// a large enough web_fetch/read_artifact result whose text a stored artifact still holds.
func stubFor(fr *genai.FunctionResponse, args map[string]any) (map[string]any, bool) {
	if fr.Response["error"] != nil || responseChars(fr.Response) < minCollapseChars {
		return nil, false
	}
	switch fr.Name {
	case toolWebFetch:
		return fetchStub(fr.Response, shapeNote(args, "pattern", "offset"))
	case toolReadArtifact:
		id, _ := args["id"].(string)
		text, _ := fr.Response["result"].(string)
		if id == "" || text == "" {
			return nil, false
		}
		return map[string]any{"result": fmt.Sprintf("[%s, %d lines read%s - collapsed from history; read_artifact(id, offset, lines) re-reads it]",
			id, strings.Count(text, "\n")+1, shapeNote(args, "offset", "lines"))}, true
	}
	return nil, false
}

// shapeNote renders the call's shaping args (e.g. ` (pattern "x", offset 40)`), so a
// stub says which slice was read, not just which artifact.
func shapeNote(args map[string]any, keys ...string) string {
	var parts []string
	for _, k := range keys {
		switch v := args[k].(type) {
		case string:
			if v != "" {
				parts = append(parts, fmt.Sprintf("%s %q", k, v))
			}
		case float64:
			if v > 0 {
				parts = append(parts, fmt.Sprintf("%s %v", k, v))
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// fetchStub keeps each entry's url/artifact/lines and drops its text; false
// when a successful entry has no artifact to point at.
func fetchStub(resp map[string]any, note string) (map[string]any, bool) {
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
			"text": fmt.Sprintf("[%s, %v lines%s - collapsed from history; read_artifact(id, offset, lines) or grep_artifacts(pattern, ids) re-reads it]", id, m["lines"], note)})
	}
	return map[string]any{"results": out}, true
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

// contentChars skips thought parts: the model adapter drops them before sending.
func contentChars(ct *genai.Content) int {
	if ct == nil {
		return 0
	}
	n := 0
	for _, p := range ct.Parts {
		switch {
		case p == nil || p.Thought:
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
