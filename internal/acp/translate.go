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
// flush check only runs when the next update arrives (acp.go's select loop isn't ours to add a ticker to), so a round that ends mid-batch with no further updates drops the trailing partial thought.
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
	meta     map[string]any
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
		p := pendingTool{kind: c.Kind, title: c.Title, rawInput: c.RawInput, content: c.Content, loc: c.Locations, meta: c.Meta}
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
		if len(up.Meta) > 0 {
			p.meta = up.Meta
		}
		if up.Status == nil || !terminalStatus(*up.Status) {
			t.pending[id] = p
			return out
		}
		delete(t.pending, id)
		name, args := t.mapToolCall(p)
		out = append(out, t.pairSpec(id, name, args, p, *up.Status == sdk.ToolCallStatusFailed, up.RawOutput))
	case u.UsageUpdate != nil:
		um := &genai.GenerateContentResponseUsageMetadata{TotalTokenCount: int32(u.UsageUpdate.Used)}
		// ACP's usage_update has no prompt/cached/completion split, so pi-acp
		// carries pi's own breakdown through the _meta extension field.
		um.PromptTokenCount = usageMetaInt(u.UsageUpdate.Meta, quackPromptTokensMetaKey)
		um.CachedContentTokenCount = usageMetaInt(u.UsageUpdate.Meta, quackCachedTokensMetaKey)
		um.CandidatesTokenCount = usageMetaInt(u.UsageUpdate.Meta, quackCompletionTokensMetaKey)
		t.usage = um
	}
	return out
}

// _meta keys pi-acp sets on usage_update, mirroring pi's own Usage shape
// (input/cacheRead/output) - see mcpMetaKey for the same _meta convention.
const (
	quackPromptTokensMetaKey     = "quack_prompt_tokens"
	quackCachedTokensMetaKey     = "quack_cached_tokens"
	quackCompletionTokensMetaKey = "quack_completion_tokens"
)

// usageMetaInt reads one numeric _meta field - JSON numbers decode as
// float64 into a map[string]any, so a plain type assertion to int fails silently.
func usageMetaInt(meta map[string]any, key string) int32 {
	if v, ok := meta[key].(float64); ok {
		return int32(v)
	}
	return 0
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

// mcpMetaKey is the _meta key an ACP agent bridging quack's own MCP tools
// (pi-acp) sets to carry the tool's real, unprefixed name - ACP's ToolKind
// enum has no slot for "this is one of quack's own tools" (#1278).
const mcpMetaKey = "quack_mcp_tool"

// mcpIdentity resolves an ACP tool call back to the real quack MCP tool name.
// pi-acp sets _meta[mcpMetaKey] directly; an agent that can't touch _meta
// (e.g. gemini-cli) still registers the tool as "<mcpServerName>_<tool>" and surfaces that as its title, so stripping the prefix there works too.
func mcpIdentity(meta map[string]any, title string) (string, bool) {
	if v, _ := meta[mcpMetaKey].(string); v != "" {
		return v, true
	}
	if name, ok := strings.CutPrefix(title, mcpServerName+"_"); ok && name != "" {
		return name, true
	}
	return "", false
}

// mapToolCall maps one ACP tool call onto quack's tool vocabulary.
func (t *translator) mapToolCall(p pendingTool) (string, map[string]any) {
	in, _ := p.rawInput.(map[string]any)
	if name, ok := mcpIdentity(p.meta, p.title); ok {
		args := map[string]any{}
		for k, v := range in {
			args[k] = v
		}
		return name, args
	}
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
		// land on grep's arg/result shape; a glob-shaped output (no "matches") still renders via GenericView instead of the richer GrepView.
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
	// A genuinely unknown kind (a third-party tool ACP has no enum slot for,
	// including the literal "other") is named after its title, never the
	// meaningless literal "other" - the frontend used to paper over this (#959) but a name the UI never has to special-case is the real fix.
	name := string(p.kind)
	useTitle := name == "" || p.kind == sdk.ToolKindOther
	if useTitle {
		name = p.title
		if name == "" {
			name = "tool"
		}
	}
	args := map[string]any{}
	if m, ok := p.rawInput.(map[string]any); ok {
		for k, v := range m {
			args[k] = v
		}
	}
	if p.title != "" && !useTitle {
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
