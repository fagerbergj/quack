package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/fagerbergj/quack/internal/schema"
)

// RunChatShow is `quack chat show <id>`: a one-screen status snapshot
// (id/title/status/pending question, the last turn's per-node table, then its
// answer text). --json prints the full ChatDetail instead. -f/--follow
// additionally attaches to the chat's live stream (Client.Subscribe) and
// prints line-oriented events until the run ends, applying the same
// pause/failure exit-code semantics as `chat send` (see Report). Returns the
// process exit code.
func RunChatShow(ctx context.Context, out, errOut io.Writer, server, id string, asJSON, follow bool) int {
	c, err := NewClient(server)
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	detail, err := c.GetChat(ctx, id)
	if err != nil {
		fmt.Fprintln(errOut, notFoundAs(err, id))
		return 1
	}
	if asJSON {
		_ = writeJSON(out, detail)
		return 0
	}
	printChatSnapshot(out, detail)
	if !follow {
		return 0
	}
	if detail.Status != schema.ChatStatusRunning {
		fmt.Fprintln(out, "(nothing running)")
		return 0
	}
	st := newStreamState()
	fs := newFollowState()
	for ev := range c.Subscribe(ctx, id) {
		fs.printLine(out, ev)
		st.handle(ev, nil)
	}
	return Report(out, errOut, id, st.result(id), false)
}

// printChatSnapshot renders a ChatDetail's status header, the last turn's
// per-node DAG table (if any), and the last turn's answer text (if any).
func printChatSnapshot(out io.Writer, d schema.ChatDetail) {
	fmt.Fprintf(out, "id:     %s\n", d.Id)
	fmt.Fprintf(out, "title:  %s\n", chatTitle(d.Title))
	fmt.Fprintf(out, "status: %s\n", d.Status)
	if d.GithubUrl != nil && *d.GithubUrl != "" {
		fmt.Fprintf(out, "github: %s\n", *d.GithubUrl)
	}
	if d.PendingQuestion != nil && *d.PendingQuestion != "" {
		fmt.Fprintf(out, "question: %s\n", *d.PendingQuestion)
	}
	if dagItem, ok := lastTurnDag(d.Turns); ok {
		fmt.Fprintln(out)
		printNodeTable(out, dagItem)
	}
	if n := len(d.Turns); n > 0 {
		if answer := strings.TrimSpace(AssistantText(d.Turns[n-1].Output)); answer != "" {
			fmt.Fprintln(out)
			fmt.Fprintln(out, answer)
		}
	}
}

// lastTurnDag returns the last turn's quack:dag output item, if any.
func lastTurnDag(turns []schema.Turn) (schema.DagOutputItem, bool) {
	if len(turns) == 0 {
		return schema.DagOutputItem{}, false
	}
	for _, it := range turns[len(turns)-1].Output {
		if d, err := it.AsDagOutputItem(); err == nil && string(d.Type) == "quack:dag" {
			return d, true
		}
	}
	return schema.DagOutputItem{}, false
}

// printNodeTable renders NODE AGENT STATUS MODEL TOKENS DURATION SCORE, in
// plan order, greppable and stable-columned (tabwriter, no ANSI).
func printNodeTable(out io.Writer, d schema.DagOutputItem) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NODE\tAGENT\tSTATUS\tMODEL\tTOKENS\tDURATION\tSCORE")
	for _, n := range d.Nodes {
		st := d.NodeStates[n.Id]
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			n.Id, n.Agent, orDash(string(st.Status)), derefStr(st.Model), formatTokens(st.TotalTokens),
			formatDurationMs(st.ServerDurationMs), formatScore(st.JudgeFinalScore))
	}
	_ = tw.Flush()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func derefStr(s *string) string {
	if s == nil || *s == "" {
		return "-"
	}
	return *s
}

func formatTokens(n *int) string {
	if n == nil {
		return "-"
	}
	return fmt.Sprintf("%d", *n)
}

func formatDurationMs(ms *int) string {
	if ms == nil {
		return "-"
	}
	if *ms < 1000 {
		return fmt.Sprintf("%dms", *ms)
	}
	return fmt.Sprintf("%.1fs", float64(*ms)/1000)
}

func formatScore(f *float64) string {
	if f == nil {
		return "-"
	}
	return fmt.Sprintf("%.2f", *f)
}

// followState carries the small amount of cross-event bookkeeping printLine
// needs so a stream of many small updates for the SAME reasoning block or tool
// call collapses to one terse line each, instead of dumping every raw event
// (#385 — collapse/summarize, the OTel traces are the full-detail surface now).
type followState struct {
	thinking map[string]bool // run_id -> already printed a "thinking…" line
	tools    map[string]bool // call_id -> already printed the call's summary line
}

func newFollowState() *followState {
	return &followState{thinking: map[string]bool{}, tools: map[string]bool{}}
}

// printLine renders one SSE event as a human-readable, line-oriented trace for
// `chat show -f` (the TUI live view's replacement): "node r1 running", a terse
// "tool: name(arg)" / "→ outcome" pair per tool call, one "thinking…" line per
// reasoning block. The orchestrator's own top-level answer text is
// deliberately NOT streamed here (unlike the old per-token print): narration
// ahead of a tool call and the eventual final answer arrive on the same
// channel, so printing tokens live has no way to "un-print" preamble once a
// later tool call reveals it wasn't the answer (#387) — the final answer,
// already reset per-tool-call by streamState (send.go), prints once via
// Report at the end of RunChatShow instead.
func (f *followState) printLine(out io.Writer, ev SSEEvent) {
	switch ev.Name {
	case "node_start":
		var d struct {
			NodeID string `json:"node_id"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			fmt.Fprintf(out, "node %s running\n", d.NodeID)
		}
	case "node_done":
		var d struct {
			NodeID string `json:"node_id"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			fmt.Fprintf(out, "node %s done\n", d.NodeID)
		}
	case "node_failed":
		var d struct {
			NodeID string `json:"node_id"`
			Error  string `json:"error"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			fmt.Fprintf(out, "node %s failed: %s\n", d.NodeID, d.Error)
		}
	case "node_needs_input":
		var d struct {
			NodeID  string `json:"node_id"`
			Message string `json:"message"`
		}
		if json.Unmarshal(ev.Data, &d) == nil {
			fmt.Fprintf(out, "node %s needs_input: %s\n", d.NodeID, d.Message)
		}
	case "agent_thinking":
		var d struct {
			NodeID string `json:"node_id"`
			RunID  string `json:"run_id"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && d.RunID != "" && !f.thinking[d.RunID] {
			f.thinking[d.RunID] = true
			fmt.Fprintf(out, "%sthinking…\n", followPrefix(d.NodeID))
		}
	case "agent_tool_call":
		var d struct {
			NodeID string         `json:"node_id"`
			CallID string         `json:"call_id"`
			Name   string         `json:"name"`
			Args   map[string]any `json:"args"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && d.Name != "" && !f.tools[d.CallID] {
			f.tools[d.CallID] = true
			fmt.Fprintf(out, "%stool: %s(%s)\n", followPrefix(d.NodeID), d.Name, summarizeToolArgs(d.Args))
		}
	case "agent_tool_result":
		var d struct {
			NodeID string `json:"node_id"`
			Name   string `json:"name"`
			Result any    `json:"result"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && d.Name != "" {
			fmt.Fprintf(out, "%s  → %s\n", followPrefix(d.NodeID), summarizeToolResult(d.Result))
		}
	}
}

// followPrefix labels a node-scoped trace line ("node n1: tool: …"); a
// top-level (orchestrator) line carries no prefix.
func followPrefix(nodeID string) string {
	if nodeID == "" {
		return ""
	}
	return "node " + nodeID + ": "
}

// summarizeToolArgs picks one representative arg to show beside the tool name
// so a call is identifiable without a full JSON dump — mirrors the priority
// order frontend/src/components/toolFormat.ts's summarizeArgs uses, for the
// same call rendered consistently across the web UI and the CLI.
func summarizeToolArgs(args map[string]any) string {
	for _, key := range []string{"query", "url", "path", "command", "message", "id", "q"} {
		if v, ok := args[key].(string); ok && v != "" {
			b, err := json.Marshal(v)
			if err == nil {
				return string(b)
			}
		}
	}
	return ""
}

// summarizeToolResult renders a tool result as one short outcome word/phrase —
// never the raw payload — matching #385's design principle that full detail
// belongs to OTel traces, not the terminal trace.
func summarizeToolResult(result any) string {
	m, ok := result.(map[string]any)
	if !ok {
		return "ok"
	}
	if e, ok := m["error"].(string); ok && e != "" {
		return "failed: " + e
	}
	if ec, ok := m["exit_code"].(float64); ok {
		return fmt.Sprintf("exit %d", int(ec))
	}
	return "ok"
}
