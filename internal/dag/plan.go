// Package dag: task planning + execution via DAG of specialist agents.
package dag

import (
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/vetting"
)

type HistoryTurn struct {
	Role string
	Text string
}

// Plan: DAG of agent tasks. Setup/Delivery are pre/post steps executed by harness.
type Plan struct {
	ID               string
	Nodes            []Node
	UserMessage      string
	History          []HistoryTurn
	Attachments      []*genai.Part
	Setup            *Setup
	Delivery         *Delivery
	Grant            *vetting.Grant
	WorkerBackground string
	CIChecks         []CICheck
}

// CICheck: one failing GitHub check, scoped per node.
type CICheck struct {
	Name   string
	Detail string
}

// Setup: declared pre-step (clone + branch). Harness-provisioned, never orchestrator-authored.
type Setup struct {
	Repo       string `json:"repo"`
	BaseRef    string `json:"base_ref"`
	WorkBranch string `json:"work_branch"`

	CheckoutExistingHead bool `json:"-"`
}

// Delivery: post-gate step for reaching GitHub, run once at the run level.
type Delivery struct {
	Kind string `json:"kind"`
}

type Node struct {
	ID        string
	AgentName string
	Task      string
	Rubric    string
	DependsOn []string
	Checks    []string
	Workdir   string
}

// terminalIDs returns IDs of nodes no other node depends on.
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
