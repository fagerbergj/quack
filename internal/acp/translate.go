package acp

import (
	"path/filepath"
	"strings"

	sdk "github.com/coder/acp-go-sdk"
	"google.golang.org/genai"
)

// eventSpec is one session event to yield: its parts, whether it is a partial
// (streamed-only, never persisted — the runner drops Partial events from the
// store) and optional usage for the final event.
type eventSpec struct {
	parts   []*genai.Part
	partial bool
	usage   *genai.GenerateContentResponseUsageMetadata
}

// translator turns ACP session/update notifications into event specs, in
// quack's tool vocabulary so the DAG stream, the activity ledger and the judge
// consume them unchanged:
//
//	agent_thought_chunk        → Partial thought-text event   (agent_thinking)
//	agent_message_chunk        → Partial text event           (agent_token), accumulated as the answer since the last tool call
//	tool_call                  → Partial FunctionCall event   (live agent_tool_call)
//	tool_call_update terminal  → durable FunctionCall+FunctionResponse pair (ledger + agent_tool_result)
//	usage_update               → retained, stamped on the final answer event
//
// The durable pair is emitted only at the TERMINAL update because opencode
// often delivers the interesting fields (the edit diff, the command output)
// there, and the ledger reads args off the call part — an early call event
// would freeze them half-empty.
type translator struct {
	cwd     string
	answer  strings.Builder
	pending map[string]pendingTool
	usage   *genai.GenerateContentResponseUsageMetadata
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
	switch {
	case u.AgentThoughtChunk != nil:
		if txt := blockText(u.AgentThoughtChunk.Content); txt != "" {
			return []eventSpec{{partial: true, parts: []*genai.Part{{Text: txt, Thought: true}}}}
		}
	case u.AgentMessageChunk != nil:
		if txt := blockText(u.AgentMessageChunk.Content); txt != "" {
			t.answer.WriteString(txt)
			return []eventSpec{{partial: true, parts: []*genai.Part{{Text: txt}}}}
		}
	case u.ToolCall != nil:
		// A tool call means everything narrated so far was pre-action
		// throat-clearing, not the answer — keep only the text emitted since.
		t.answer.Reset()
		c := u.ToolCall
		id := string(c.ToolCallId)
		p := pendingTool{kind: c.Kind, title: c.Title, rawInput: c.RawInput, content: c.Content, loc: c.Locations}
		t.pending[id] = p
		name, args := t.mapToolCall(p)
		if terminalStatus(c.Status) {
			delete(t.pending, id)
			return []eventSpec{t.pairSpec(id, name, args, p, c.Status == sdk.ToolCallStatusFailed, c.RawOutput)}
		}
		// Live progress only — the durable pair lands at the terminal update.
		return []eventSpec{{partial: true, parts: []*genai.Part{{FunctionCall: &genai.FunctionCall{ID: id, Name: name, Args: args}}}}}
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
			return nil
		}
		delete(t.pending, id)
		name, args := t.mapToolCall(p)
		return []eventSpec{t.pairSpec(id, name, args, p, *up.Status == sdk.ToolCallStatusFailed, up.RawOutput)}
	case u.UsageUpdate != nil:
		t.usage = &genai.GenerateContentResponseUsageMetadata{TotalTokenCount: int32(u.UsageUpdate.Used)}
	}
	// plan / available_commands / mode / config / session_info and unknown
	// variants: skipped, never fatal (minor ACP versions add variants).
	return nil
}

// finalSpec is the round's durable answer event: the agent message text
// accumulated since the last tool call (what RunNode[string] returns via the
// node Output) — earlier narration was reset away at each ToolCall dispatch.
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

// mapToolCall maps one ACP tool call onto quack's tool vocabulary. Only the
// ledger-relevant kinds are renamed; everything else keeps a kind-derived name
// that the stream shows and the ledger ignores.
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
	}
	name := string(p.kind)
	if name == "" {
		name = "tool"
	}
	return name, map[string]any{"title": p.title}
}

// toolResponse builds the FunctionResponse payload. An "error" key marks the
// operation FAILED to the ledger (recordWsOp), exactly like a native tool.
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

// editPath resolves the edited file's path (diff content first, then
// locations), node-relative — the namespace the ledger and the judge's
// changed-file re-read resolve against.
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
// node-relative one. A path outside the node dir is kept verbatim — the jail
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

// outputText extracts human-readable output from tool content blocks or a raw
// output payload.
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
