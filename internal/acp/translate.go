package acp

import (
	"path/filepath"
	"strings"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	"google.golang.org/genai"
)

// eventSpec is one session event to yield.
type eventSpec struct {
	parts   []*genai.Part
	partial bool
	usage   *genai.GenerateContentResponseUsageMetadata
}

// Thinking deltas arrive one per streamed token - raw, that's a DB row per
// token. Coalesce into batches flushed on whichever limit hits first; the
// flush check only runs when the next update arrives (acp.go's select loop
// isn't ours to add a ticker to), so a round that ends mid-batch with no
// further updates drops the trailing partial thought.
const (
	thinkFlushElapsed = 1500 * time.Millisecond
	thinkFlushBytes   = 750
)

// translator turns ACP session/update notifications into event specs in
// quack's tool vocabulary.
type translator struct {
	cwd       string
	answer    strings.Builder
	pending   map[string]pendingTool
	usage     *genai.GenerateContentResponseUsageMetadata
	thinking  strings.Builder
	thinkOpen time.Time // zero when no batch is open
}

type pendingTool struct {
	kind     sdk.ToolKind
	title    string
	rawInput any
	content  []sdk.ToolCallContent
	loc      []sdk.ToolCallLocation
}

func newTranslator(cwd string) *translator {
	return &translator{cwd: cwd, pending: map[string]pendingTool{}}
}

func (t *translator) translate(u sdk.SessionUpdate) []eventSpec {
	if u.AgentThoughtChunk != nil {
		return t.bufferThought(u.AgentThoughtChunk)
	}

	// Any non-thinking update closes out a batch in progress - ordering is
	// preserved since the flushed thought always precedes this update's own spec.
	var out []eventSpec
	if spec, ok := t.flushThought(); ok {
		out = append(out, spec)
	}

	switch {
	case u.AgentMessageChunk != nil:
		if txt := blockText(u.AgentMessageChunk.Content); txt != "" {
			t.answer.WriteString(txt)
			out = append(out, eventSpec{partial: true, parts: []*genai.Part{{Text: txt}}})
		}
	case u.ToolCall != nil:
		t.answer.Reset()
		c := u.ToolCall
		id := string(c.ToolCallId)
		p := pendingTool{kind: c.Kind, title: c.Title, rawInput: c.RawInput, content: c.Content, loc: c.Locations}
		t.pending[id] = p
		name, args := t.mapToolCall(p)
		if terminalStatus(c.Status) {
			delete(t.pending, id)
			out = append(out, t.pairSpec(id, name, args, p, c.Status == sdk.ToolCallStatusFailed, c.RawOutput))
		} else {
			out = append(out, eventSpec{partial: true, parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: id, Name: name, Args: args}}}})
		}
	case u.ToolCallUpdate != nil:
		up := u.ToolCallUpdate
		id := string(up.ToolCallId)
		p := t.pending[id] // zero value on create-on-update (spec-sanctioned agent behaviour)
		if up.Kind != nil {
			p.kind = *up.Kind
		}
		if up.Title != nil {
			p.title = *up.Title
		}
		if up.RawInput != nil {
			p.rawInput = up.RawInput
		}
		if len(up.Content) > 0 {
			p.content = up.Content
		}
		if len(up.Locations) > 0 {
			p.loc = up.Locations
		}
		if up.Status == nil || !terminalStatus(*up.Status) {
			t.pending[id] = p
			return out
		}
		delete(t.pending, id)
		name, args := t.mapToolCall(p)
		out = append(out, t.pairSpec(id, name, args, p, *up.Status == sdk.ToolCallStatusFailed, up.RawOutput))
	case u.UsageUpdate != nil:
		t.usage = &genai.GenerateContentResponseUsageMetadata{TotalTokenCount: int32(u.UsageUpdate.Used)}
	}
	return out
}

// bufferThought appends one thinking delta to the open batch, flushing it
// once either coalescing limit is hit.
func (t *translator) bufferThought(c *sdk.SessionUpdateAgentThoughtChunk) []eventSpec {
	txt := blockText(c.Content)
	if txt == "" {
		return nil
	}
	if t.thinkOpen.IsZero() {
		t.thinkOpen = time.Now()
	}
	t.thinking.WriteString(txt)
	if time.Since(t.thinkOpen) >= thinkFlushElapsed || t.thinking.Len() >= thinkFlushBytes {
		spec, _ := t.flushThought()
		return []eventSpec{spec}
	}
	return nil
}

// flushThought drains the open thinking batch, if any.
func (t *translator) flushThought() (eventSpec, bool) {
	if t.thinking.Len() == 0 {
		return eventSpec{}, false
	}
	txt := t.thinking.String()
	t.thinking.Reset()
	t.thinkOpen = time.Time{}
	return eventSpec{partial: true, parts: []*genai.Part{{Text: txt, Thought: true}}}, true
}

// finalSpec is the round's durable answer event: the agent message text
// accumulated since the last tool call (what RunNode[string] returns via the
// node Output) - earlier narration was reset away at each ToolCall dispatch.
func finalSpec(t *translator) eventSpec {
	return eventSpec{parts: []*genai.Part{{Text: t.answer.String()}}, usage: t.usage}
}

func terminalStatus(s sdk.ToolCallStatus) bool {
	return s == sdk.ToolCallStatusCompleted || s == sdk.ToolCallStatusFailed
}

// pairSpec builds the durable call+response event for one finished tool call.
// Parts are ordered call-then-response so the ledger's pairing scan works
// within the single event.
func (t *translator) pairSpec(id, name string, args map[string]any, p pendingTool, failed bool, rawOutput any) eventSpec {
	resp := t.toolResponse(name, p, failed, rawOutput)
	return eventSpec{parts: []*genai.Part{
		{FunctionCall: &genai.FunctionCall{ID: id, Name: name, Args: args}},
		{FunctionResponse: &genai.FunctionResponse{ID: id, Name: name, Response: resp}},
	}}
}

// mapToolCall maps one ACP tool call onto quack's tool vocabulary.
func (t *translator) mapToolCall(p pendingTool) (string, map[string]any) {
	in, _ := p.rawInput.(map[string]any)
	switch p.kind {
	case sdk.ToolKindExecute:
		cmd, _ := in["command"].(string)
		if cmd == "" {
			cmd = p.title
		}
		return "run_command", map[string]any{"command": cmd}
	case sdk.ToolKindEdit:
		if d := firstDiff(p.content); d != nil {
			old := ""
			if d.OldText != nil {
				old = *d.OldText
			}
			return "edit_file", map[string]any{"path": t.rel(d.Path), "old": old, "new": d.NewText}
		}
		if path := t.editPath(p); path != "" {
			return "write_file", map[string]any{"path": path}
		}
	case sdk.ToolKindRead:
		if path := t.firstPath(p); path != "" {
			return "read_file", map[string]any{"path": path}
		}
	case sdk.ToolKindFetch:
		url, _ := in["url"].(string)
		if url == "" {
			url = p.title
		}
		return "web_fetch", map[string]any{"url": url}
	case sdk.ToolKindDelete:
		if path := t.firstPath(p); path != "" {
			return "delete_path", map[string]any{"path": path}
		}
	case sdk.ToolKindSearch:
		// ACP's "search" kind covers both content search and filename glob -
		// the protocol carries no tool-identity field to split them, so both
		// land on grep's arg/result shape; a glob-shaped output (no "matches")
		// still renders via GenericView instead of the richer GrepView.
		args := map[string]any{}
		if pattern, _ := in["pattern"].(string); pattern != "" {
			args["pattern"] = pattern
		} else if p.title != "" {
			args["pattern"] = p.title
		}
		if path := t.firstPath(p); path != "" {
			args["path"] = path
		}
		if g, _ := in["glob"].(string); g != "" {
			args["glob"] = g
		} else if inc, _ := in["include"].(string); inc != "" {
			args["glob"] = inc
		}
		return "grep", args
	}
	name := string(p.kind)
	if name == "" {
		name = "tool"
	}
	args := map[string]any{}
	if m, ok := p.rawInput.(map[string]any); ok {
		for k, v := range m {
			args[k] = v
		}
	}
	if p.title != "" {
		args["title"] = p.title
	}
	return name, args
}

// toolResponse builds the FunctionResponse payload.
func (t *translator) toolResponse(name string, p pendingTool, failed bool, rawOutput any) map[string]any {
	if failed {
		msg := outputText(p, rawOutput)
		if msg == "" {
			msg = p.title
		}
		return map[string]any{"error": bound(msg, 2000)}
	}
	out, _ := rawOutput.(map[string]any)
	switch name {
	case "run_command":
		resp := map[string]any{"exit_code": exitCode(out)}
		if txt := outputText(p, rawOutput); txt != "" {
			resp["output"] = bound(txt, 2000)
		}
		return resp
	case "edit_file":
		return map[string]any{"replacements": 1}
	case "delete_path":
		return map[string]any{"deleted": true}
	case "write_file":
		resp := map[string]any{}
		if d := firstDiff(p.content); d != nil {
			resp["bytes"] = len(d.NewText)
			resp["created"] = d.OldText == nil
		}
		return resp
	case "read_file":
		resp := map[string]any{}
		if txt := outputText(p, rawOutput); txt != "" {
			resp["content"] = bound(txt, 2000)
		}
		return resp
	default:
		resp := map[string]any{}
		if txt := outputText(p, rawOutput); txt != "" {
			resp["output"] = bound(txt, 2000)
		}
		return resp
	}
}

// editPath resolves the edited file's path, node-relative.
func (t *translator) editPath(p pendingTool) string {
	if d := firstDiff(p.content); d != nil {
		return t.rel(d.Path)
	}
	return t.firstPath(p)
}

func (t *translator) firstPath(p pendingTool) string {
	if len(p.loc) > 0 {
		return t.rel(p.loc[0].Path)
	}
	if in, ok := p.rawInput.(map[string]any); ok {
		for _, k := range []string{"filePath", "file_path", "path"} {
			if s, _ := in[k].(string); s != "" {
				return t.rel(s)
			}
		}
	}
	return ""
}

// rel converts the agent's ABSOLUTE path (ACP mandates absolute paths) to a
// node-relative one. A path outside the node dir is kept verbatim - the jail
// resolve downstream refuses it, which is the right failure.
func (t *translator) rel(p string) string {
	if t.cwd == "" || !filepath.IsAbs(p) {
		return p
	}
	r, err := filepath.Rel(t.cwd, p)
	if err != nil || strings.HasPrefix(r, "..") {
		return p
	}
	return r
}

func firstDiff(content []sdk.ToolCallContent) *sdk.ToolCallContentDiff {
	for _, c := range content {
		if c.Diff != nil {
			return c.Diff
		}
	}
	return nil
}

// outputText extracts human-readable output from tool content or raw output.
func outputText(p pendingTool, rawOutput any) string {
	var b strings.Builder
	for _, c := range p.content {
		if c.Content != nil {
			if txt := blockText(c.Content.Content); txt != "" {
				b.WriteString(txt)
			}
		}
	}
	if b.Len() > 0 {
		return b.String()
	}
	if out, ok := rawOutput.(map[string]any); ok {
		for _, k := range []string{"output", "stdout", "message"} {
			if s, _ := out[k].(string); s != "" {
				return s
			}
		}
	}
	return ""
}

func exitCode(out map[string]any) int {
	for _, k := range []string{"exit", "exitCode", "exit_code", "code"} {
		switch v := out[k].(type) {
		case float64:
			return int(v)
		case int:
			return v
		}
	}
	return 0
}

func blockText(b sdk.ContentBlock) string {
	if b.Text != nil {
		return b.Text.Text
	}
	return ""
}

func bound(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…[truncated]"
}
