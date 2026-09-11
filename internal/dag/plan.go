// Package dag: task planning + execution via DAG of specialist agents.
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
	// ResumedFrom: this node's dag_node ContextID at plan-build time, set
	// only when the assignment named an existing (terminal-status) node id -
	// "" for a freshly minted node. Seeds an ACP node's session/load and
	// drives the "continues" ledger/UI signal for both transports; a native
	// node resumes automatically off its own stable A2A contextID regardless.
	ResumedFrom string
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
