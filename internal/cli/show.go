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
	if detail.Status != schema.Running {
		fmt.Fprintln(out, "(nothing running)")
		return 0
	}
	st := newStreamState()
	for ev := range c.Subscribe(ctx, id) {
		printFollowLine(out, ev)
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
			n.Id, n.Agent, orDash(st.Status), derefStr(st.Model), formatTokens(st.TotalTokens),
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

// printFollowLine renders one SSE event as a human-readable, line-oriented
// trace for `chat show -f` (the TUI live view's replacement): "node r1
// running", "node r1 needs_input: <question>", answer text as it lands.
func printFollowLine(out io.Writer, ev SSEEvent) {
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
	case "agent_token":
		var d struct {
			NodeID string `json:"node_id"`
			Text   string `json:"text"`
		}
		if json.Unmarshal(ev.Data, &d) == nil && d.NodeID == "" && d.Text != "" {
			fmt.Fprint(out, d.Text)
		}
	}
}
