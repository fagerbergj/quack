// Package dag plans and executes tasks as a DAG of specialist agents.
package dag

import (
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
	// AllowedDeliveryKinds: nil = unrestricted; non-nil (even empty) restricts staged delivery to exactly
	// these kinds, the same sentinel as vetting.Config.AllowedDeliveryKinds.
	AllowedDeliveryKinds []string
	WorkerBackground     string
	ContextItems         []ContextItem
	// PlanOnly: the deliverable is a plan, not a change. Stamped by the harness from the triggering label,
	// never model-authored; forces every node read-only with no delivery target.
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

	// Provisioned: set once Executor.Provision cloned this Setup, so a second call is a no-op instead
	// of a double clone. Not in resume-plan JSON: setup never re-runs on resume.
	Provisioned bool `json:"-"`
}

// Delivery: post-gate step for reaching GitHub, run once at the run level.
type Delivery struct {
	Kind string `json:"kind"`
}

// Node.ContextWindow is the assigned agent's configured limit, which the context meter compares live usage against.
type Node struct {
	ID            string
	AgentName     string
	Task          string
	Rubric        string
	DependsOn     []string
	Checks        []string
	Workdir       string
	ContextWindow int
	// Artifact: episodic record name this node writes on gate pass.
	Artifact string
	// ResumedFrom: an existing terminal node's dag_node ContextID, "" for a fresh node. Seeds an ACP node's
	// session/load and drives the "continues" signal; a native node resumes off its stable A2A contextID anyway.
	ResumedFrom string
	// Result: this node's output from an earlier execute step ("" if not yet run), so the plan judge
	// can see what already happened.
	Result string
	// Status: the earlier step's outcome (Assignment.Status), "" for a node not yet run.
	Status string
}

// TerminalIDs are the plan's sinks - nodes nothing depends on - in plan order; each one's output is delivered.
func TerminalIDs(nodes []Node) []string {
	return SinksAmong(nodes, func(string) bool { return true })
}

// SinksAmong are the nodes in the set that no other node in it depends on, in plan order.
func SinksAmong(nodes []Node, in func(string) bool) []string {
	hasSuccessor := map[string]bool{}
	for _, n := range nodes {
		if !in(n.ID) {
			continue
		}
		for _, dep := range n.DependsOn {
			hasSuccessor[dep] = true
		}
	}
	var out []string
	for _, n := range nodes {
		if in(n.ID) && !hasSuccessor[n.ID] {
			out = append(out, n.ID)
		}
	}
	return out
}
