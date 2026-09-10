// Package dag: task planning + execution via DAG of specialist agents.
package dag

import (
	"fmt"
	"strings"

	"google.golang.org/genai"
)

type HistoryTurn struct {
	Role string
	Text string
}

// Plan: DAG of agent tasks. Setup/Delivery are pre/post steps executed by harness.
type Plan struct {
	ID          string
	Nodes       []Node
	UserMessage string
	History     []HistoryTurn
	Attachments []*genai.Part
	Setup       *Setup
	Delivery    *Delivery
	// AllowedDeliveryKinds: nil = unrestricted (no trigger governs this run);
	// non-nil (including empty) restricts staged delivery to exactly these
	// kinds - see vetting.Config.AllowedDeliveryKinds, the same sentinel.
	AllowedDeliveryKinds []string
	WorkerBackground     string
	ContextItems         []ContextItem
	// PlanOnly: this run's deliverable is a plan, not a change (#739). Stamped
	// by the harness from the triggering label, never model-authored - forces
	// every node read-only with no delivery target regardless of agent (buildGateNodes).
	PlanOnly bool
}

// ContextItem: name-keyed detail injected into any node whose task names it
// (e.g. one failing CI check, scoped to the node fixing it).
type ContextItem struct {
	Name   string
	Detail string
}

// Setup: declared pre-step (clone + branch). Harness-provisioned, never orchestrator-authored.
type Setup struct {
	Repo       string `json:"repo"`
	BaseRef    string `json:"base_ref"`
	WorkBranch string `json:"work_branch"`

	CheckoutExistingHead bool `json:"-"`

	// Provisioned: set once Executor.Provision has cloned this Setup - makes a
	// second Provision/runPlanSetup call (execute tool, then run phase) a no-op
	// instead of a double clone. Skips resume-plan JSON: setup never re-runs on resume anyway.
	Provisioned bool `json:"-"`
}

// Delivery: post-gate step for reaching GitHub, run once at the run level.
type Delivery struct {
	Kind string `json:"kind"`
}

// Node.ContextWindow is stamped from the assigned agent's config at plan
// assembly - the static limit the context meter compares live usage against.
type Node struct {
	ID            string
	AgentName     string
	Task          string
	Rubric        string
	DependsOn     []string
	Checks        []string
	Workdir       string
	ContextWindow int
	// Artifact: episodic record name this node writes on gate pass (#1006).
	Artifact string
	// Continue: a prior (possibly earlier-turn) node id whose agent session
	// this node's first round resumes, when the executor's eligibility and
	// freshness checks pass. "" = fresh node, the default.
	Continue string
}

// SessionHandle is the durable pointer a terminal node leaves behind so a
// later turn's continue: can resume its agent session: the ADK session id
// (native agents - always the chat's own session) or the ACP protocol
// session id (external agents, from session/new), plus the workspace scope
// and agent name the continue primitive's eligibility check compares
// against, and the shared clone's HEAD at the moment this node went
// terminal (the freshness baseline).
type SessionHandle struct {
	Kind    string `json:"kind"` // "adk" or "acp"
	ID      string `json:"id"`
	Agent   string `json:"agent"`
	Scope   string `json:"scope"`
	HeadSHA string `json:"head_sha"`
	// Branch/IsolationScope: the ADK invocation-context coordinates the
	// node's own events were tagged with ("adk" kind only) - a later
	// continue: reruns under these exact values so ADK's own history
	// replay (branch prefix match + isolation-scope exact match) includes
	// what this node said, instead of a prompt-text splice.
	Branch         string `json:"branch,omitempty"`
	IsolationScope string `json:"isolation_scope,omitempty"`
	// Repo/BaseRef/WorkBranch/DeliveryKind: the plan-level setup/delivery this
	// node's own plan declared - a later plan that continues this node
	// inherits them when it leaves its own setup/delivery unset (see
	// tools.NewPlanTool's inheritContinuedSetupDelivery), so a follow-up that
	// only extends this node's work never has to restate a repo/branch it
	// never saw.
	Repo         string `json:"repo,omitempty"`
	BaseRef      string `json:"base_ref,omitempty"`
	WorkBranch   string `json:"work_branch,omitempty"`
	DeliveryKind string `json:"delivery_kind,omitempty"`
}

// ResumableNode is one candidate the plan tool surfaces to the orchestrator:
// a terminal node from the chat's last turn a follow-up's continue: may name.
type ResumableNode struct {
	ID      string `json:"id"`
	Agent   string `json:"agent"`
	Summary string `json:"summary"`
	// Setup/Delivery: the prior plan's own declared setup/delivery, carried
	// so a continuing plan can inherit them - see SessionHandle's matching
	// fields, which is where these are actually persisted.
	Setup    *Setup    `json:"-"`
	Delivery *Delivery `json:"-"`
}

// ResumableNodesDesc renders resumable as a stated fact for the orchestrator's
// OWN turn content (like AttachmentDesc) - not just the `plan` tool's static
// schema description, which a smaller model reading a long tool description
// can fail to act on. "" when resumable is empty.
func ResumableNodesDesc(resumable []ResumableNode) string {
	if len(resumable) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("FACT: this chat has resumable node(s) from the last turn's plan - if this message refines " +
		"or extends one of their work, set that node's `continue` field to its id in your `plan` call instead of " +
		"starting a fresh node for it:")
	for _, r := range resumable {
		fmt.Fprintf(&sb, "\n- %s (agent: %s): %s", r.ID, r.Agent, r.Summary)
	}
	return sb.String()
}

func terminalIDs(nodes []Node) []string {
	hasSuccessor := map[string]bool{}
	for _, n := range nodes {
		for _, dep := range n.DependsOn {
			hasSuccessor[dep] = true
		}
	}
	var out []string
	for _, n := range nodes {
		if !hasSuccessor[n.ID] {
			out = append(out, n.ID)
		}
	}
	return out
}
