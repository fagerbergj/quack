package orchestrator

import (
	"context"
	"fmt"
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/tools"
)

// needsFormatPass reports whether a plan's terminal output is raw specialist
// text that never passed through a synthesizer - the case #430 found
// inconsistent: the SAME request could come back as a formatted plan or a
// bare exploration report purely on how the planner happened to word the
// terminal node's task. The plan-work skill now guides the planner to add a
// synthesizer whenever the deliverable's shape matters; this is the
// orchestrator-side fallback for when it didn't (or couldn't, on a lone
// node). Skipped when the plan already ends in a synthesizer (already
// formatted, see agents/synthesizer/prompt.md) or declares a GitHub delivery
// - there the terminal code-implementer/code-reviewer node stages its own
// PR/review per-node (vetting.commitDelivery), and this chat text is not
// the deliverable.
func needsFormatPass(plan dag.Plan) bool {
	if plan.Delivery != nil {
		return false
	}
	term := terminalNode(plan.Nodes)
	return term != nil && term.AgentName != "synthesizer"
}

// terminalNode returns the plan node no other node depends on (nil if nodes
// is empty). Mirrors dag.terminalIDs/tools.TerminalOutput's own walk, but
// this package can't import the unexported one and needs the NODE (its
// AgentName), not just its output text.
func terminalNode(nodes []dag.Node) *dag.Node {
	hasSuccessor := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		for _, dep := range n.DependsOn {
			hasSuccessor[dep] = true
		}
	}
	for i := range nodes {
		if !hasSuccessor[nodes[i].ID] {
			return &nodes[i]
		}
	}
	return nil
}

// formatterInstruction keeps the pass reshaping, never re-researching: the
// writer has no tools and sees only the text it's handed.
const formatterInstruction = `You reformat a specialist agent's raw output into a clear, well-organized answer to the user's request. You have no tools and do no new research - work ONLY from the text given to you.

Rules:
- Preserve every fact, citation, file reference, and conclusion in the input - never drop or invent content.
- Organize with Markdown headings/lists where that helps a multi-part answer; leave a short, single-point answer as plain prose.
- If the user's request asks for a plan, make sure the output reads as one (phases or steps), not a bare report of findings.
- Do not narrate your process ("Let me format...", "Here is the reformatted answer:") - output the answer directly, starting with its content.
- If the input is already clear and well-organized, return it with only minimal changes.`

// formatAnswer runs a tool-less, gate-less pass over a plan's raw terminal
// output so the deliverable reads as a coherent answer regardless of which
// specialist produced it. Same recipe as vetting.runWriterFresh: a fresh
// in-memory runner, no tools, no session persistence - it never redoes the
// work, only reshapes text already produced. Fails open: any error, or an
// empty model, returns the raw answer unchanged, since a broken format pass
// must never block delivery.
func formatAnswer(ctx context.Context, m model.LLM, message, answer string) string {
	answer = strings.TrimSpace(answer)
	if m == nil || answer == "" {
		return answer
	}
	writer, err := llmagent.New(llmagent.Config{
		Name:        "orchestrator-format",
		Description: "Formats a specialist's raw output into a clear, well-structured answer.",
		Model:       m,
		Instruction: formatterInstruction,
	})
	if err != nil {
		return answer
	}
	r, err := runner.New(runner.Config{
		AppName: "quack-format", Agent: writer,
		SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		return answer
	}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: fmt.Sprintf(
		"User's request:\n%s\n\nSpecialist's raw output to format:\n%s", message, answer)}}}
	var out strings.Builder
	for ev, rerr := range r.Run(ctx, "format", "format", content, adkagent.RunConfig{}) {
		if rerr != nil {
			return answer
		}
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && !p.Thought && p.Text != "" {
				out.WriteString(p.Text)
			}
		}
	}
	if formatted := strings.TrimSpace(stream.StripThinking(out.String())); formatted != "" {
		return formatted
	}
	return answer
}

// finalizeAnswer is the single place every delivery path (Run, RetryNode,
// resumeNodeRun) turns a finished plan's node outputs into the text that gets
// persisted and shown to the user - so the format-pass fallback (#430)
// applies uniformly rather than being wired into each call site separately.
func (o *Orchestrator) finalizeAnswer(ctx context.Context, plan dag.Plan, nodeOutputs map[string]string) string {
	answer := tools.TerminalOutput(plan, nodeOutputs)
	if !needsFormatPass(plan) {
		return answer
	}
	return formatAnswer(ctx, o.model, plan.UserMessage, answer)
}
