package orchestrator

import (
	"context"
	"fmt"
	"regexp"
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

// listItemPattern: a Markdown bullet or ordered-list item at line start.
var listItemPattern = regexp.MustCompile(`(?m)^\s*(?:[-*+]|\d+[.)])\s+\S`)

// alreadyStructured reports whether answer already reads as organized output:
// a Markdown heading, or two-plus list items - the two shapes the format
// pass's own instruction says to add ("Organize with Markdown headings/lists").
func alreadyStructured(answer string) bool {
	if strings.Contains(answer, "\n#") || strings.HasPrefix(answer, "#") {
		return true
	}
	return len(listItemPattern.FindAllStringIndex(answer, 2)) >= 2
}

// needsFormatPass: true when raw specialist output needs a format pass (no
// synthesizer, no GitHub delivery, and not already structured) - an answer
// that already reads as organized gains nothing from a second full decode,
// regardless of its length.
func needsFormatPass(plan dag.Plan, answer string) bool {
	if plan.Delivery != nil {
		return false
	}
	term := terminalNode(plan.Nodes)
	if term == nil || term.AgentName == "synthesizer" {
		return false
	}
	return !alreadyStructured(answer)
}

// terminalNode returns the terminal node (nil if empty). Replicates dag.terminalIDs' walk for AgentName.
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

// formatAnswer runs a tool-less/gate-less format pass; fails open (never blocks delivery).
func formatAnswer(ctx context.Context, m model.LLM, message, answer, chatID string) string {
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
	// Real chat id groups this run under its causing chat in Langfuse
	// (gen_ai.conversation.id); empty falls back rather than emitting "".
	sessionID := "format"
	if chatID != "" {
		sessionID = chatID
	}
	var out strings.Builder
	for ev, rerr := range r.Run(ctx, "format", sessionID, content, adkagent.RunConfig{}) {
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

// finalizeAnswer: single place every delivery path formats node outputs for the user.
func (o *Orchestrator) finalizeAnswer(ctx context.Context, plan dag.Plan, nodeOutputs map[string]string, chatID string) string {
	answer := tools.TerminalOutput(plan, nodeOutputs)
	if !needsFormatPass(plan, answer) {
		return answer
	}
	return formatAnswer(ctx, o.model, plan.UserMessage, answer, chatID)
}
