package tui

import (
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/fagerbergj/quack/internal/stream"
)

type nodeStatus int

const (
	statusPending nodeStatus = iota // planned, not yet queued
	statusQueued                    // ready, waiting for a slot
	statusRunning                   // executing now
	statusDone                      // completed (gate passed)
	statusFailed                    // gate failed / errored
)

type dagNode struct {
	id      string
	agent   string
	task    string
	deps    []string
	status  nodeStatus
	failErr string
	output  string // node's full vetted text (from node_done)
}

// dagState is the live DAG for one run: the planned nodes plus their evolving
// status as node_* events arrive.
type dagState struct {
	nodes []dagNode
	index map[string]int // node id → position in nodes (plan order)
}

func newDAG(d stream.DagPlanData) *dagState {
	ds := &dagState{index: make(map[string]int, len(d.Nodes))}
	for _, n := range d.Nodes {
		ds.index[n.ID] = len(ds.nodes)
		ds.nodes = append(ds.nodes, dagNode{id: n.ID, agent: n.Agent, task: n.Task, deps: n.DependsOn, status: statusPending})
	}
	return ds
}

func (d *dagState) set(id string, s nodeStatus) {
	if i, ok := d.index[id]; ok {
		d.nodes[i].status = s
	}
}

func (d *dagState) fail(id, msg string) {
	if i, ok := d.index[id]; ok {
		d.nodes[i].status = statusFailed
		d.nodes[i].failErr = msg
	}
}

func (d *dagState) setOutput(id, out string) {
	if i, ok := d.index[id]; ok {
		d.nodes[i].output = out
	}
}

// terminalOutput returns the output of the terminal node (no successors) — the
// run's answer in deliver mode (execute end_turn=true), where the orchestrator
// streams no top-level tokens. Falls back to the last node that produced output.
func (d *dagState) terminalOutput() string {
	if d == nil {
		return ""
	}
	hasSuccessor := make(map[string]bool, len(d.nodes))
	for _, n := range d.nodes {
		for _, dep := range n.deps {
			hasSuccessor[dep] = true
		}
	}
	for _, n := range d.nodes {
		if !hasSuccessor[n.id] && strings.TrimSpace(n.output) != "" {
			return n.output
		}
	}
	for i := len(d.nodes) - 1; i >= 0; i-- {
		if strings.TrimSpace(d.nodes[i].output) != "" {
			return d.nodes[i].output
		}
	}
	return ""
}

func (d *dagState) counts() (done, failed, total int) {
	for _, n := range d.nodes {
		total++
		switch n.status {
		case statusDone:
			done++
		case statusFailed:
			failed++
		}
	}
	return
}

// depth is the longest dependency chain ending at id — used to indent nodes into
// layers so the DAG reads top-down. memo guards against repeated work; a missing
// dep (id not in the plan) contributes 0, so a malformed plan still renders.
func (d *dagState) depth(id string, memo map[string]int) int {
	if v, ok := memo[id]; ok {
		return v
	}
	memo[id] = 0 // break cycles defensively
	i, ok := d.index[id]
	if !ok {
		return 0
	}
	best := 0
	for _, dep := range d.nodes[i].deps {
		if dd := d.depth(dep, memo) + 1; dd > best {
			best = dd
		}
	}
	memo[id] = best
	return best
}

// render draws the DAG: one node per line, indented by dependency depth, with a
// status icon (spin is the current spinner frame for running nodes). width caps
// each line (ANSI-aware) so long tasks don't wrap and break the layout.
func (d *dagState) render(spin string, width int) string {
	if d == nil || len(d.nodes) == 0 {
		return ""
	}
	memo := make(map[string]int, len(d.nodes))
	var b strings.Builder
	done, failed, total := d.counts()
	header := mutedStyle.Render("plan")
	if failed > 0 {
		header += "  " + errStyle.Render(strconv.Itoa(failed)+" failed")
	}
	b.WriteString(header + "  " + faintStyle.Render(strconv.Itoa(done)+"/"+strconv.Itoa(total)+" done") + "\n")
	for _, n := range d.nodes {
		indent := strings.Repeat("  ", d.depth(n.id, memo))
		icon, st := nodeIcon(n.status, spin)
		label := n.agent
		if label == "" {
			label = n.id
		}
		line := indent + icon + " " + st.Render(label)
		if task := strings.TrimSpace(n.task); task != "" {
			line += mutedStyle.Render(" — " + firstLine(task))
		}
		if n.status == statusFailed && n.failErr != "" {
			line += " " + errStyle.Render("("+firstLine(n.failErr)+")")
		}
		if width > 0 {
			line = ansi.Truncate(line, width, "…")
		}
		b.WriteString(line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func nodeIcon(s nodeStatus, spin string) (string, lipgloss.Style) {
	switch s {
	case statusRunning:
		return runStyle.Render(spin), runStyle
	case statusDone:
		return okStyle.Render("✓"), okStyle
	case statusFailed:
		return errStyle.Render("✗"), errStyle
	case statusQueued:
		return mutedStyle.Render("◔"), mutedStyle
	default:
		return faintStyle.Render("○"), mutedStyle
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
