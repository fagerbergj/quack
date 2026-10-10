package dag

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/decide"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// PlanRejectedError: the plan judge declined a proposed plan. Reason is the judge's internal text:
// never surface it as a user-facing answer; it belongs in logs and the ledger.
type PlanRejectedError struct {
	Reason string
}

func (e *PlanRejectedError) Error() string {
	return fmt.Sprintf("this plan was rejected: %s\nFix the nodes and call plan again.", e.Reason)
}

const implementerAgent = "code-implementer"
const (
	reviewerAgent    = "code-reviewer"
	synthesizerAgent = "synthesizer"
)
const explorerAgent = "code-explorer"

// agentDeliveryKind maps an agent whose work IS a delivery mechanism to the only Delivery.Kind it can
// reach GitHub through. Hardcoded names: nothing machine-readable declares this.
var agentDeliveryKind = map[string]string{
	reviewerAgent:    "review",
	implementerAgent: "pull_request",
}

// RequiredDeliveryKind reports the Delivery.Kind agent's job is coupled to (ok=false if none), so
// create_plan/edit_plan can reject an undeliverable hire before the node burns tokens.
func RequiredDeliveryKind(agent string) (kind string, ok bool) {
	kind, ok = agentDeliveryKind[agent]
	return kind, ok
}

const reviewChurnThreshold = 800

var changedChurnRe = regexp.MustCompile(`\(\+(\d+)/-(\d+)\)`)

func totalChurn(message string) int {
	sum := 0
	for _, m := range changedChurnRe.FindAllStringSubmatch(message, -1) {
		a, _ := strconv.Atoi(m[1])
		d, _ := strconv.Atoi(m[2])
		sum += a + d
	}
	return sum
}

type AgentInfo struct {
	Name        string
	Description string
	// ContextWindow: the agent's configured context_window (0 if unset), carried onto each node for the
	// frontend's context meter.
	ContextWindow int
	// DefaultArtifact: the bundle-declared default output artifact kind ("" if unset); assemble() stamps
	// it onto every node assigned this agent.
	DefaultArtifact string
}

// Planner validates an orchestrator-authored DAG and stamps turn context for the executor.
type Planner struct {
	agents        []AgentInfo
	checkCommands []string
	judge         vetting.PlanJudge
	decisions     *decide.Decider
}

// planAccept asks one holistic question (per-criterion questions calibrated badly for plans). In decide
// mode only a confident accept skips the judge: a reject was never validated on bad plans.
var planAccept = decide.Register(decide.Point{
	ID:      "plan.accept",
	Primary: "accept",
	Questions: map[string]decide.Question{"accept": {Type: "noul", Instructions: "Should an independent reviewer ACCEPT this " +
		"plan step as the right next move for the user's request (it is not clearly the wrong shape)? Unfinished plans " +
		"with no delivery declared are acceptable."}},
	Restrictive: []string{"false"},
	Acts:        []string{"true"},
	Replaces:    "plan_judge",
	Modes:       []string{config.DecisionModeObserve, config.DecisionModeDecide},
}, func(top string) bool { return top == "true" })

// NewPlanner returns a Planner over the agent roster, check prefixes, and plan judge.
func NewPlanner(agents []AgentInfo, checkCommands []string, judge vetting.PlanJudge) *Planner {
	SetAgentRoster(agents)
	SetCheckCommands(checkCommands)
	return &Planner{agents: agents, checkCommands: checkCommands, judge: judge}
}

// SetDecisions attaches the decision intercept points; nil (the default) disables them.
func (p *Planner) SetDecisions(d *decide.Decider) { p.decisions = d }

func (p *Planner) CheckCommands() []string { return p.checkCommands }

// RawNode is one DAG node Build/BuildBound assembles into a Plan, from a dag_plan record's
// assignments (execute) or a bound workflow shape's config.
type RawNode struct {
	ID        string   `json:"id"`
	Agent     string   `json:"agent"`
	Task      string   `json:"task"`
	Rubric    string   `json:"rubric,omitempty"`
	DependsOn []string `json:"depends_on"`
	Checks    []string `json:"checks,omitempty"`
	Workdir   string   `json:"workdir,omitempty"`
	// Artifact: registered recordstore kind this node's output is saved as on gate pass; assemble
	// validates it or falls back to the agent's bundle-declared default.
	Artifact    string `json:"artifact,omitempty"`
	ResumedFrom string `json:"resumed_from,omitempty"`
	Result      string `json:"result,omitempty"`
	Status      string `json:"status,omitempty"`
}

// ValidateArtifactKind rejects an artifact selector outside the registered recordstore kinds.
// config calls recordstore directly to avoid an import cycle (dag -> inference -> config).
func ValidateArtifactKind(kind string) error { return recordstore.ValidateArtifactKind(kind) }

// AssignmentsToRawNodes converts a dag_plan record's assignments into Build's input, resolving each
// agent from nodeAgent and each prior ContextID from resumedFrom (missing = fresh node). Errors on an unknown node id.
func AssignmentsToRawNodes(assignments []Assignment, nodeAgent, resumedFrom map[string]string) ([]RawNode, error) {
	out := make([]RawNode, 0, len(assignments))
	for _, a := range assignments {
		agent, ok := nodeAgent[a.NodeID]
		if !ok {
			return nil, fmt.Errorf("assignments.%s: no dag_node record for this node id", a.NodeID)
		}
		out = append(out, RawNode{
			ID: a.NodeID, Agent: agent, Task: a.Task, Rubric: a.Rubric,
			DependsOn: a.DependsOn, Checks: a.Checks, Workdir: a.Workdir,
			ResumedFrom: resumedFrom[a.NodeID], Result: a.Result, Status: a.Status(),
		})
	}
	return out, nil
}

// Build validates submitted nodes into a Plan and stamps turn context.
func (p *Planner) Build(ctx context.Context, nodes []RawNode, setup *Setup, delivery *Delivery, history []HistoryTurn, message string, attachments []*genai.Part, allowedKinds []string) (plan *Plan, err error) {
	ctx, span := otelobs.Start(ctx, "plan")
	defer func() { otelobs.End(span, err) }()

	infos := p.infosFor(ctx)
	plan, err = assemble(nodes, infos, p.checkCommands, setup, delivery, allowedKinds)
	if err != nil {
		return nil, err
	}
	span.SetAttributes(attribute.String(otelobs.GenAIWorkflowName, plan.ID), attribute.Int("node_count", len(plan.Nodes)))
	if err = checkReviewDeliverable(plan); err != nil {
		return nil, err
	}
	if err = p.judgeRouting(ctx, plan, message); err != nil {
		return nil, err
	}
	if p.judge == nil {
		if err = checkReviewFanout(infos, plan, message); err != nil {
			return nil, err
		}
	}
	plan.History = history
	plan.UserMessage = message
	plan.Attachments = attachments
	return plan, nil
}

// BuildBound builds a Plan from a workflow-catalog shape's fixed node list: Build's structural
// validation, but no plan judge or fan-out heuristic, since the shape was validated at config load.
func (p *Planner) BuildBound(ctx context.Context, nodes []RawNode, setup *Setup, delivery *Delivery, message string, attachments []*genai.Part, allowedKinds []string) (plan *Plan, err error) {
	_, span := otelobs.Start(ctx, "plan.bound")
	defer func() { otelobs.End(span, err) }()

	plan, err = assemble(nodes, p.infosFor(ctx), p.checkCommands, setup, delivery, allowedKinds)
	if err != nil {
		return nil, err
	}
	// ponytail: one sink only - a bound run has no dag_plan record to fill a retried sink's siblings
	// from; allow several once retry/resume can seed them from the completed nodes' outputs.
	if sinks := TerminalIDs(plan.Nodes); len(sinks) > 1 {
		return nil, fmt.Errorf("bound workflow has %d terminal nodes (%s); a bound shape must end in one node", len(sinks), strings.Join(sinks, ", "))
	}
	span.SetAttributes(attribute.String(otelobs.GenAIWorkflowName, plan.ID), attribute.Int("node_count", len(plan.Nodes)))
	plan.UserMessage = message
	plan.Attachments = attachments
	return plan, nil
}

// infosFor: the pinned roster's agents (Executor.Pin), else NewPlanner's; also covers NewExecutor's
// Gen 0 placeholder, which carries no Infos.
func (p *Planner) infosFor(ctx context.Context) []AgentInfo {
	if r := pinnedRoster(ctx); r != nil && r.Gen > 0 {
		return r.Infos
	}
	return p.agents
}

type judgeWaivedKey struct{}

// WithPlanJudgeWaived skips the plan judge for plans built under ctx: the user chose to run a plan
// the judge kept rejecting.
func WithPlanJudgeWaived(ctx context.Context) context.Context {
	return context.WithValue(ctx, judgeWaivedKey{}, true)
}

func (p *Planner) judgeRouting(ctx context.Context, plan *Plan, message string) error {
	if p.judge == nil {
		return nil
	}
	if waived, _ := ctx.Value(judgeWaivedKey{}).(bool); waived {
		slog.Info("plan judge waived by the user's choice", "component", "planner", "plan", plan.ID)
		return nil
	}
	ctx, span := otelobs.Start(ctx, "plan.judge")
	defer span.End()

	var repoKey string
	if plan.Setup != nil {
		repoKey = workspace.NormalizeRepoURL(plan.Setup.Repo)
	}
	settle, accepted := p.planAccept(ctx, plan, message, repoKey)
	if accepted {
		span.SetAttributes(attribute.Bool("accept", true), attribute.String("skipped_by", planAccept.ID))
		return nil
	}
	accept, reason, err := p.judge(ctx, message, planSummary(plan), repoKey)
	settle(judgeVerdict(accept, reason, err))
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
	emitPlanRejectedEvent(ctx, plan, reason)
	return &PlanRejectedError{Reason: reason}
}

// planAccept starts plan.accept; in decide mode it waits and reports a confident accept, which skips
// the judge (a sampled audit still runs it). settle records the judge's verdict otherwise.
func (p *Planner) planAccept(ctx context.Context, plan *Plan, message, repoKey string) (func(string) <-chan struct{}, bool) {
	state := "User's request:\n" + message + "\n\nProposed plan:\n" + renderPlan(plan, true)
	if p.decisions.Mode(planAccept.ID) != config.DecisionModeDecide {
		return p.decisions.Observe(ctx, planAccept.Point, state), false
	}
	r, settle := p.decisions.Await(ctx, planAccept.Point, state)
	if !r.Act() {
		return settle, false
	}
	slog.Info("plan judge skipped: plan.accept accepted the plan", "component", "planner", "plan", plan.ID,
		"top_p", r.TopP, "latency_ms", r.Latency.Milliseconds())
	if !p.decisions.Audit(planAccept.ID) {
		settle("")
		return nil, true
	}
	ctx = context.WithoutCancel(ctx)
	summary := planSummary(plan)
	go func() { settle(judgeVerdict(p.judge(ctx, message, summary, repoKey))) }()
	return nil, true
}

// judgeVerdict is the judge's answer in plan.accept's option space, "" when it failed.
func judgeVerdict(accept bool, _ string, err error) string {
	if err != nil {
		return ""
	}
	return strconv.FormatBool(accept)
}

// emitPlanRejectedEvent records the judge's rejection reason to the ledger verbatim: the durable
// trail for text that must never reach the user-facing reply.
func emitPlanRejectedEvent(ctx context.Context, plan *Plan, reason string) {
	if !otelobs.LoggingEnabled("quack.planner") {
		return
	}
	otelobs.EmitLog(ctx, "quack.planner", "",
		attribute.String(otelobs.GenAIOperationName, otelobs.GenAIOperationPlanRejected),
		attribute.String(otelobs.GenAIWorkflowName, plan.ID),
		attribute.String(otelobs.GenAIEvaluationExplain, reason),
	)
}

// resultPreviewLen caps how much of an already-run node's result the judge sees.
const resultPreviewLen = 600

func planSummary(p *Plan) string { return renderPlan(p, false) }

// renderPlan is the judge's summary, or with facts plan.accept's state: earlier nodes' outputs
// reduced to status, artifact kind and size, and no quack-authored hints.
func renderPlan(p *Plan, facts bool) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d node(s):", len(p.Nodes))
	for _, n := range p.Nodes {
		fmt.Fprintf(&sb, "\n- %s (%s)", n.ID, n.AgentName)
		if len(n.DependsOn) > 0 {
			fmt.Fprintf(&sb, " depends on %s", strings.Join(n.DependsOn, ", "))
		}
		fmt.Fprintf(&sb, "\n    task: %s", strings.TrimSpace(n.Task))
		if facts {
			writeNodeFacts(&sb, n)
		} else if r := strings.TrimSpace(n.Result); r != "" {
			if len(r) > resultPreviewLen {
				r = r[:resultPreviewLen] + "…"
			}
			fmt.Fprintf(&sb, "\n    ALREADY RAN, result: %s", r)
		}
	}
	if p.Setup != nil {
		fmt.Fprintf(&sb, "\nsetup: repo=%q base_ref=%q work_branch=%q", p.Setup.Repo, p.Setup.BaseRef, p.Setup.WorkBranch)
	} else {
		sb.WriteString("\nsetup: (none declared)")
	}
	switch {
	case p.Delivery != nil:
		fmt.Fprintf(&sb, "\ndelivery: kind=%q", p.Delivery.Kind)
	case facts:
		sb.WriteString("\ndelivery: none")
	default:
		sb.WriteString("\ndelivery: (none declared - this step may be a partial plan, more nodes to follow)")
	}
	return sb.String()
}

func writeNodeFacts(sb *strings.Builder, n Node) {
	fmt.Fprintf(sb, "\n    status: %s", cmp.Or(n.Status, "pending"))
	if n.Artifact != "" {
		fmt.Fprintf(sb, ", artifact kind: %s", n.Artifact)
	}
	if n.Status != "" {
		fmt.Fprintf(sb, ", output: %d bytes", len(n.Result))
	}
}

// checkReviewDeliverable runs unconditionally: the LLM plan judge once accepted a review dispatch with
// no reviewer node. Fires only off an explicitly declared plan.Delivery; a step may just be partial.
func checkReviewDeliverable(plan *Plan) error {
	// ponytail: hardcoded agent name (no machine-readable review capability exists); a code-reviewer
	// node passes even if its task never calls stage_review. Derive from agent cards if that matters.
	if plan.Delivery == nil || plan.Delivery.Kind != "review" {
		return nil
	}
	for _, n := range plan.Nodes {
		if n.AgentName == reviewerAgent {
			return nil
		}
	}
	return fmt.Errorf("a review deliverable needs a %s node: only a %s node can stage inline comments and a "+
		"verdict via the review MCP tools (stage_review/stage_review_comment) - this plan has none, so no formal "+
		"review could ever be staged. Add a terminal %s node.", reviewerAgent, reviewerAgent, reviewerAgent)
}

// checkReviewFanout: rejects single-reviewer plans for large PRs (judge-disabled fallback).
func checkReviewFanout(agents []AgentInfo, plan *Plan, message string) error {
	hasExplorer := false
	for _, a := range agents {
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

// AttachmentDesc describes attachment MIME types for the text-only orchestrator. Attachments arrive
// as artifactref FileData parts; InlineData is checked too for raw callers.
func AttachmentDesc(parts []*genai.Part) string {
	if len(parts) == 0 {
		return ""
	}
	var mimes []string
	for _, p := range parts {
		switch {
		case p.InlineData != nil && p.InlineData.MIMEType != "":
			mimes = append(mimes, p.InlineData.MIMEType)
		case p.FileData != nil && p.FileData.MIMEType != "":
			mimes = append(mimes, p.FileData.MIMEType)
		}
	}
	if len(mimes) == 0 {
		return ""
	}
	return fmt.Sprintf("[User attached: %d file(s): %s]", len(mimes), strings.Join(mimes, ", "))
}

// assemble validates nodes, hardens synthesizer deps, checks acyclicity, validates delivery kind.
func assemble(nodes []RawNode, agents []AgentInfo, checkCommands []string, setup *Setup, delivery *Delivery, allowedKinds []string) (*Plan, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("plan has no nodes")
	}
	delivery, err := resolveDelivery(delivery, allowedKinds)
	if err != nil {
		return nil, err
	}
	known := make(map[string]AgentInfo, len(agents))
	for _, a := range agents {
		known[a.Name] = a
	}
	ids := make(map[string]bool, len(nodes))
	plan := &Plan{ID: uuid.NewString(), Setup: setup, Delivery: delivery, AllowedDeliveryKinds: allowedKinds}
	for _, n := range nodes {
		node, err := buildNode(n, known, checkCommands, ids)
		if err != nil {
			return nil, err
		}
		plan.Nodes = append(plan.Nodes, node)
	}

	plan.Nodes = hardenSynthesizer(plan.Nodes, known)

	if _, topoErr := topoLayers(*plan); topoErr != nil {
		return nil, topoErr
	}
	if err := validateRepoChain(*plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// validateChecks enforces prefix-matching against workspace.check_commands.
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

var deliveryKinds = map[string]bool{"pull_request": true, "review": true, "comment": true}

// DefaultDeliveryFromAllowedKinds resolves a kindless delivery when the dispatch grants exactly one
// kind; nil when unrestricted or several, so the model must name it.
func DefaultDeliveryFromAllowedKinds(allowed []string) *Delivery {
	if len(allowed) != 1 || !deliveryKinds[allowed[0]] {
		return nil
	}
	return &Delivery{Kind: allowed[0]}
}

func validateDelivery(d *Delivery) error {
	if d == nil {
		return nil
	}
	if !deliveryKinds[d.Kind] {
		return fmt.Errorf("delivery.kind %q must be one of pull_request, review, comment", d.Kind)
	}
	return nil
}

// didYouMean: a near-miss hint only when exactly one candidate is within edit distance 2.
func didYouMean(got string, candidates []string) string {
	var best string
	matches := 0
	for _, c := range candidates {
		if d := editDistance(got, c); d <= 2 {
			best = c
			matches++
		}
	}
	if matches == 1 {
		return fmt.Sprintf(" (did you mean %q?)", best)
	}
	return ""
}

// editDistance: Levenshtein, used only for short agent/node names.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

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

// resolveDelivery infers a kindless delivery from a single-kind dispatch; an omitted delivery means
// "not yet" and is never inferred.
func resolveDelivery(delivery *Delivery, allowedKinds []string) (*Delivery, error) {
	if delivery != nil && delivery.Kind == "" {
		def := DefaultDeliveryFromAllowedKinds(allowedKinds)
		if def == nil {
			return nil, fmt.Errorf("delivery.kind: required - this dispatch allows more than one delivery kind (%s), so it can't be inferred", strings.Join(allowedKinds, ", "))
		}
		delivery = def
	}
	if err := validateDelivery(delivery); err != nil {
		return nil, err
	}
	return delivery, nil
}

// buildNode validates one raw node against the dispatch's agents/commands and shapes it.
func buildNode(n RawNode, known map[string]AgentInfo, checkCommands []string, ids map[string]bool) (Node, error) {
	if n.ID == "" {
		return Node{}, fmt.Errorf("node missing id")
	}
	if ids[n.ID] {
		return Node{}, fmt.Errorf("duplicate node id %q", n.ID)
	}
	agentInfo, ok := known[n.Agent]
	if !ok {
		names := slices.Sorted(maps.Keys(known))
		return Node{}, fmt.Errorf("unknown agent %q for node %q; valid agents: %s%s",
			n.Agent, n.ID, strings.Join(names, ", "), didYouMean(n.Agent, names))
	}
	if len(n.Checks) > 0 {
		if err := validateChecks(n.Checks, checkCommands); err != nil {
			return Node{}, fmt.Errorf("node %q: %w", n.ID, err)
		}
	}
	if n.Artifact != "" {
		if err := ValidateArtifactKind(n.Artifact); err != nil {
			return Node{}, fmt.Errorf("node %q: %w", n.ID, err)
		}
	}
	// n.Artifact (a config-bound workflow node) overrides the agent's bundle-declared default.
	artifactKind := n.Artifact
	if artifactKind == "" {
		artifactKind = agentInfo.DefaultArtifact
	}
	ids[n.ID] = true
	return Node{
		ID:            n.ID,
		AgentName:     n.Agent,
		Task:          n.Task,
		Rubric:        n.Rubric,
		DependsOn:     n.DependsOn,
		Checks:        n.Checks,
		Workdir:       n.Workdir,
		ContextWindow: agentInfo.ContextWindow,
		Artifact:      artifactKind,
		ResumedFrom:   n.ResumedFrom,
		Result:        n.Result,
		Status:        n.Status,
	}, nil
}

// hardenSynthesizer: the synthesizer depends on every non-synthesizer node not downstream of it. Only a
// review fan-out (2+ reviewers) without one gets a synthesizer appended, to stage the overall verdict.
func hardenSynthesizer(nodes []Node, known map[string]AgentInfo) []Node {
	reviewers, hasSynth := 0, false
	for i, n := range nodes {
		switch n.AgentName {
		case reviewerAgent:
			reviewers++
		case synthesizerAgent:
			hasSynth = true
			nodes[i].DependsOn = synthDeps(nodes, n.ID)
		}
	}
	synthInfo, hasSynthAgent := known[synthesizerAgent]
	if hasSynth || reviewers < 2 || !hasSynthAgent {
		return nodes
	}
	all := make([]string, 0, len(nodes))
	for _, n := range nodes {
		all = append(all, n.ID)
	}
	return append(nodes, Node{
		ID:            "synthesize",
		AgentName:     synthesizerAgent,
		Task:          "Combine the reviewers' findings into one review and stage its single overall verdict.",
		DependsOn:     all,
		ContextWindow: synthInfo.ContextWindow,
	})
}

func synthDeps(nodes []Node, synthID string) []string {
	down := descendants(nodes, synthID)
	var deps []string
	for _, m := range nodes {
		if m.ID != synthID && m.AgentName != synthesizerAgent && !down[m.ID] {
			deps = append(deps, m.ID)
		}
	}
	return deps
}
