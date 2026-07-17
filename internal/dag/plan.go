// Package dag implements DAG-based task planning and execution for M3.
// The planner decomposes a user request into a directed acyclic graph of
// specialist-agent tasks; the executor runs them in topological order,
// in parallel where possible, and streams progress events.
package dag

import (
	"google.golang.org/genai"
)

// HistoryTurn is one prior conversation turn, passed to nodes as native ADK
// session events so the model sees real user/model turns rather than a
// flattened transcript.
type HistoryTurn struct {
	Role string // genai role: "user" or "model"
	Text string
}

// Plan is a DAG of agent tasks for one user request. UserMessage is the user's
// request verbatim and History the prior conversation — both flow to every
// node so specialists see the full context, not just the planner's compressed
// task description. Attachments carries media parts (images, audio) from the
// current turn; they are threaded to nodes whose agents declare image/audio inputs.
//
// Setup and Delivery are the orchestrator's DECLARED pre/post steps — the
// harness EXECUTES them, deterministically and App-authed; no node runs git,
// push, or GitHub-mutating calls itself. Both are optional: a plan with no
// GitHub repo involved (plain research/chat) sets neither.
type Plan struct {
	ID          string
	Nodes       []Node
	UserMessage string
	History     []HistoryTurn
	Attachments []*genai.Part
	Setup       *Setup
	Delivery    *Delivery
}

// Setup is the plan's declared PRE-step: the working clone + branch the
// harness provisions before any node runs. The orchestrator names the repo,
// ref, and branch; it never calls git_clone/git_branch for the triggering
// repo itself.
type Setup struct {
	Repo       string `json:"repo"`
	BaseRef    string `json:"base_ref"`
	WorkBranch string `json:"work_branch"`
}

// Delivery is the plan's declared POST-step: how the gated result reaches
// GitHub, run AFTER the trust gate passes — once, at the run level, never by
// a node mid-run. The orchestrator declares only the KIND ("pull_request",
// "review", or "comment"; see validateDelivery) — the implementer authors
// the PR title+body itself via stage_pr.
type Delivery struct {
	Kind string `json:"kind"`
}

// Node is one task in the plan: the agent to run, what to do, an acceptance
// rubric for the judge, and which other nodes' outputs this node depends on.
type Node struct {
	ID        string
	AgentName string
	Task      string
	Rubric    string
	DependsOn []string // IDs of predecessor nodes
	// Checks are orchestrator-set deterministic gate commands (see
	// .quack/plan-pr5-tool-schemas.md §4) the trust gate runs against this
	// node's output — plan-time validated (Planner.Build / assemble) against
	// workspace.check_commands; empty for every node that doesn't opt in
	// (research, synthesis).
	Checks []string
	// Workdir is the workspace-relative directory Checks run in (the node's
	// repo, e.g. "repo" after a git_clone). Ignored when Checks is empty.
	Workdir string
}

// terminalIDs returns the IDs of nodes no other node depends on — the plan's
// terminal (output-producing) nodes. A runnable native graph has exactly one
// (see buildPlanGraph); the planner uses this to append a synthesizer fan-in
// when the model omitted it.
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
