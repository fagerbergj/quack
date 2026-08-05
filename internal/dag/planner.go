package dag

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

const implementerAgent = "code-implementer"
const reviewerAgent = "code-reviewer"
const explorerAgent = "code-explorer"

// reviewChurnThreshold: max lines before reviewer must fan out.
const reviewChurnThreshold = 800

// changedChurnRe: matches "(+add/-del)" churn markers in webhook summaries.
var changedChurnRe = regexp.MustCompile(`\(\+(\d+)/-(\d+)\)`)

// totalChurn: sums added+deleted lines in the changed-files summary.
func totalChurn(message string) int {
	sum := 0
	for _, m := range changedChurnRe.FindAllStringSubmatch(message, -1) {
		a, _ := strconv.Atoi(m[1])
		d, _ := strconv.Atoi(m[2])
		sum += a + d
	}
	return sum
}

// AgentInfo describes one available agent for the orchestrator.
type AgentInfo struct {
	Name        string
	Description string
}

// Planner validates an orchestrator-authored DAG and stamps turn context for the executor.
type Planner struct {
	agents        []AgentInfo
	checkCommands []string
	judge         vetting.PlanJudge
}

// NewPlanner: returns a Planner over the agent roster, check prefixes, and plan judge.
func NewPlanner(agents []AgentInfo, checkCommands []string, judge vetting.PlanJudge) *Planner {
	return &Planner{agents: agents, checkCommands: checkCommands, judge: judge}
}

// CheckCommands: configured check-command prefixes.
func (p *Planner) CheckCommands() []string { return p.checkCommands }

// RawNode is one DAG node the orchestrator submits to the plan tool.
type RawNode struct {
	ID        string   `json:"id"`
	Agent     string   `json:"agent"`
	Task      string   `json:"task"`
	Rubric    string   `json:"rubric,omitempty"`
	DependsOn []string `json:"depends_on"`
	Checks    []string `json:"checks,omitempty"`
	Workdir   string   `json:"workdir,omitempty"`
}

// Build: validates submitted nodes into a Plan and stamps turn context.
func (p *Planner) Build(ctx context.Context, nodes []RawNode, setup *Setup, delivery *Delivery, history []HistoryTurn, message string, attachments []*genai.Part, grant *vetting.Grant) (plan *Plan, err error) {
	ctx, span := otelobs.Start(ctx, "plan")
	defer func() { otelobs.End(span, err) }()

	plan, err = assemble(nodes, p.agents, p.checkCommands, setup, delivery, grant)
	if err != nil {
		return nil, err
	}
	span.SetAttributes(attribute.String("plan_id", plan.ID), attribute.Int("node_count", len(plan.Nodes)))
	if err = p.judgeRouting(ctx, plan, message); err != nil {
		return nil, err
	}
	if p.judge == nil {
		if err = p.checkReviewFanout(plan, message); err != nil {
			return nil, err
		}
	}
	plan.History = history
	plan.UserMessage = message
	plan.Attachments = attachments
	return plan, nil
}

// judgeRouting: scores plan shape against request via the plan judge.
func (p *Planner) judgeRouting(ctx context.Context, plan *Plan, message string) error {
	if p.judge == nil {
		return nil
	}
	ctx, span := otelobs.Start(ctx, "plan.judge")
	defer span.End()

	accept, reason, err := p.judge(ctx, message, planSummary(plan))
	if err != nil {
		span.RecordError(err)
		slog.Warn("plan judge unavailable, allowing plan", "component", "planner", "error", err)
		return nil
	}
	span.SetAttributes(attribute.Bool("accept", accept))
	if accept {
		return nil
	}
	slog.Warn("plan rejected by plan judge", "component", "planner", "reason", reason, "message", message)
	return fmt.Errorf("this plan was rejected: %s\nFix the nodes and call plan again.", reason)
}

// planSummary: renders the plan for the plan judge.
func planSummary(p *Plan) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d node(s):", len(p.Nodes))
	for _, n := range p.Nodes {
		fmt.Fprintf(&sb, "\n- %s (%s)", n.ID, n.AgentName)
		if len(n.DependsOn) > 0 {
			fmt.Fprintf(&sb, " depends on %s", strings.Join(n.DependsOn, ", "))
		}
		fmt.Fprintf(&sb, "\n    task: %s", strings.TrimSpace(n.Task))
	}
	if p.Setup != nil {
		fmt.Fprintf(&sb, "\nsetup: repo=%q base_ref=%q work_branch=%q", p.Setup.Repo, p.Setup.BaseRef, p.Setup.WorkBranch)
	} else {
		sb.WriteString("\nsetup: (none declared)")
	}
	if p.Delivery != nil {
		fmt.Fprintf(&sb, "\ndelivery: kind=%q", p.Delivery.Kind)
	} else {
		sb.WriteString("\ndelivery: (none declared)")
	}
	return sb.String()
}

// checkReviewFanout: rejects single-reviewer plans for large PRs (judge-disabled fallback).
func (p *Planner) checkReviewFanout(plan *Plan, message string) error {
	hasExplorer := false
	for _, a := range p.agents {
		if a.Name == explorerAgent {
			hasExplorer = true
			break
		}
	}
	if !hasExplorer || totalChurn(message) < reviewChurnThreshold {
		return nil
	}
	var reviewers, explorers int
	for _, n := range plan.Nodes {
		switch n.AgentName {
		case reviewerAgent:
			reviewers++
		case explorerAgent:
			explorers++
		}
	}
	if reviewers == 0 || explorers > 0 {
		return nil // not a review plan, or already fanned out
	}
	slog.Warn("plan rejected: large PR review not fanned out",
		"component", "planner", "churn", totalChurn(message), "threshold", reviewChurnThreshold)
	return fmt.Errorf("this PR is large (%d changed lines, over the %d-line threshold) and a single %s node will "+
		"choke on the whole diff. Split the review: read the changed-files list in the request, group the files into "+
		"slices of roughly 300 changed lines each, and add ONE %s node per slice (its task: review ONLY the files in "+
		"its slice, gather findings, do NOT post). Keep ONE %s node that depends on all the explorers, validates their "+
		"pooled findings against the diff, and posts. Do NOT keep a lone %s node.",
		totalChurn(message), reviewChurnThreshold, reviewerAgent, explorerAgent, reviewerAgent, reviewerAgent)
}

// AttachmentDesc: description of attachment MIME types for the text-only orchestrator.
func AttachmentDesc(parts []*genai.Part) string {
	if len(parts) == 0 {
		return ""
	}
	var mimes []string
	for _, p := range parts {
		if p.InlineData != nil && p.InlineData.MIMEType != "" {
			mimes = append(mimes, p.InlineData.MIMEType)
		}
	}
	if len(mimes) == 0 {
		return ""
	}
	return fmt.Sprintf("[User attached: %d file(s): %s]", len(mimes), strings.Join(mimes, ", "))
}

// assemble: validates nodes, hardens synthesizer deps, checks acyclicity, validates delivery kind.
func assemble(nodes []RawNode, agents []AgentInfo, checkCommands []string, setup *Setup, delivery *Delivery, grant *vetting.Grant) (*Plan, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("plan has no nodes")
	}
	if err := validateDelivery(delivery); err != nil {
		return nil, err
	}
	known := make(map[string]bool, len(agents))
	for _, a := range agents {
		known[a.Name] = true
	}
	ids := make(map[string]bool, len(nodes))
	plan := &Plan{ID: uuid.NewString(), Setup: setup, Delivery: delivery, Grant: grant}
	for _, n := range nodes {
		if n.ID == "" {
			return nil, fmt.Errorf("node missing id")
		}
		if ids[n.ID] {
			return nil, fmt.Errorf("duplicate node id %q", n.ID)
		}
		if !known[n.Agent] {
			return nil, fmt.Errorf("unknown agent %q for node %q", n.Agent, n.ID)
		}
		if len(n.Checks) > 0 {
			if err := validateChecks(n.Checks, checkCommands); err != nil {
				return nil, fmt.Errorf("node %q: %w", n.ID, err)
			}
		}
		ids[n.ID] = true
		plan.Nodes = append(plan.Nodes, Node{
			ID:        n.ID,
			AgentName: n.Agent,
			Task:      n.Task,
			Rubric:    n.Rubric,
			DependsOn: n.DependsOn,
			Checks:    n.Checks,
			Workdir:   n.Workdir,
		})
	}

	// Harden: synthesizer depends on every non-synthesizer node NOT downstream of it.
	if len(plan.Nodes) > 1 {
		hasSynth := false
		for _, n := range plan.Nodes {
			if n.AgentName == "synthesizer" {
				hasSynth = true
				break
			}
		}
		for i, n := range plan.Nodes {
			if n.AgentName != "synthesizer" {
				continue
			}
			down := descendants(plan.Nodes, n.ID)
			var deps []string
			for _, m := range plan.Nodes {
				if m.ID == n.ID || m.AgentName == "synthesizer" || down[m.ID] {
					continue
				}
				deps = append(deps, m.ID)
			}
			plan.Nodes[i].DependsOn = deps
		}
		// Append a synthesizer fan-in when the orchestrator omits it and multi-terminal would fail.
		if !hasSynth && known["synthesizer"] && len(terminalIDs(plan.Nodes)) > 1 {
			// Appended fan-in is safe: nothing depends on it, no descendants to cycle into.
			var all []string
			for _, n := range plan.Nodes {
				all = append(all, n.ID)
			}
			plan.Nodes = append(plan.Nodes, Node{
				ID:        "synthesize",
				AgentName: "synthesizer",
				Task:      "Combine the findings from every preceding node into one complete, well-cited answer to the user's request.",
				DependsOn: all,
			})
		}
	}

	if _, err := topoLayers(*plan); err != nil {
		return nil, err
	}
	if err := validateRepoChain(*plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// validateChecks: enforces prefix-matching against workspace.check_commands.
func validateChecks(checks, checkCommands []string) error {
	if len(checkCommands) == 0 {
		return fmt.Errorf("checks are unavailable (workspace.check_commands is empty) - omit `checks`")
	}
	for _, c := range checks {
		c = strings.TrimSpace(c)
		if c == "" {
			return fmt.Errorf("empty check command")
		}
		// No metachar rejection: checks run shell-less, prefix allowlist is the boundary.
		if !workspace.MatchesCheckPrefix(c, checkCommands) {
			return fmt.Errorf("check %q does not match any configured workspace.check_commands prefix (%s)",
				c, strings.Join(checkCommands, ", "))
		}
	}
	return nil
}

// deliveryKinds: constrained post-gate vocabulary.
var deliveryKinds = map[string]bool{"pull_request": true, "review": true, "comment": true}

// validateDelivery: rejects delivery kinds outside the constrained vocabulary.
func validateDelivery(d *Delivery) error {
	if d == nil {
		return nil
	}
	if !deliveryKinds[d.Kind] {
		return fmt.Errorf("delivery.kind %q must be one of pull_request, review, comment", d.Kind)
	}
	return nil
}

// descendants: nodes transitively depending on id (downstream).
func descendants(nodes []Node, id string) map[string]bool {
	dependents := map[string][]string{} // dep -> nodes that depend on it
	for _, n := range nodes {
		for _, d := range n.DependsOn {
			dependents[d] = append(dependents[d], n.ID)
		}
	}
	out := map[string]bool{}
	stack := []string{id}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, next := range dependents[cur] {
			if !out[next] {
				out[next] = true
				stack = append(stack, next)
			}
		}
	}
	return out
}
