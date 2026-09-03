// Package workflowcatalog composes deployment-defined DAG shapes
// (config.Config.Workflows, quack.yaml's top-level workflows:) into the
// plan-work skill's "Common workflows" table (issue #805), so an operator
// teaches the planner a house-standard shape without forking the shipped
// skill. Composition happens once, at skill-source construction (startup):
// the planner always sees one deterministic table, never a mid-plan lookup
// that can fail or drift between rounds of the same run.
package workflowcatalog

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/dag"
)

// planWorkSkill is the only skill this package augments.
const planWorkSkill = "plan-work"

// tableSep anchors the Common workflows table's header separator; every
// contiguous "|"-prefixed line below it, until the first blank line, is a
// row of that table.
const tableSep = "| --- | --- |"

// Shape is a composed catalog entry with provenance - the record shape a
// future dynamic store (#806) will persist. Source is always "operator" and
// Approved always true today because quack.yaml is the only source and being
// in the file IS the approval; the fields exist now so #806 swaps storage,
// not the record.
type Shape struct {
	Name     string
	Trigger  string
	DAGShape string
	Agents   []string
	Source   string
	Version  string
	Approved bool
	// Nodes is non-empty only for a bound shape (workflow binding): Bind
	// renders it into a dag.Plan directly, skipping the planner LLM call
	// entirely. Empty Nodes means Trigger/DAGShape stay a planner hint only,
	// exactly like every shape before binding existed.
	Nodes []config.WorkflowNode
}

// FromConfig builds provenance-carrying Shapes from config entries.
// config.Config.validateWorkflows has already dropped malformed entries,
// confirmed every named agent is configured, and validated any bound node
// list, so this is a pure mapping.
func FromConfig(shapes []config.WorkflowShape, revision string) []Shape {
	out := make([]Shape, 0, len(shapes))
	for _, w := range shapes {
		out = append(out, Shape{
			Name: w.Name, Trigger: w.Trigger, DAGShape: w.Shape, Agents: w.Agents,
			Source: "operator", Version: revision, Approved: true, Nodes: w.Nodes,
		})
	}
	return out
}

// Lookup returns the shape named name, if any of shapes matches.
func Lookup(shapes []Shape, name string) (Shape, bool) {
	for _, s := range shapes {
		if s.Name == name {
			return s, true
		}
	}
	return Shape{}, false
}

// askPlaceholder is the only substitution a bound node's task template
// supports - deliberately no templating engine, per the design's "minimal"
// call: the first (and only) consumer is a one-or-two-node ingest pipeline.
const askPlaceholder = "{{ask}}"

// Bind renders shape's bound nodes into dag.RawNode, substituting ask for
// every {{ask}} token in each node's task. ok is false when shape has no
// bound nodes - still a planner hint only, never a programmatic binding.
func Bind(shape Shape, ask string) (nodes []dag.RawNode, ok bool) {
	if len(shape.Nodes) == 0 {
		return nil, false
	}
	out := make([]dag.RawNode, len(shape.Nodes))
	for i, n := range shape.Nodes {
		out[i] = dag.RawNode{
			ID:        n.ID,
			Agent:     n.Agent,
			Task:      strings.ReplaceAll(n.Task, askPlaceholder, ask),
			Rubric:    n.Rubric,
			DependsOn: n.DependsOn,
			Artifact:  n.Artifact,
		}
	}
	return out, true
}

// Wrap returns src unchanged when shapes is empty - the regression guard: a
// deployment with no custom shapes gets the exact same Source, not a
// passthrough wrapper around it, so the composed catalog is byte-identical
// to today's. Otherwise it returns a Source that appends shapes to
// plan-work's table on every LoadInstructions("plan-work") call.
func Wrap(src skill.Source, shapes []Shape) skill.Source {
	if len(shapes) == 0 {
		return src
	}
	return &augmented{Source: src, shapes: shapes}
}

type augmented struct {
	skill.Source
	shapes []Shape
}

func (a *augmented) LoadInstructions(ctx context.Context, name string) (string, error) {
	instructions, err := a.Source.LoadInstructions(ctx, name)
	if err != nil || name != planWorkSkill {
		return instructions, err
	}
	return compose(instructions, a.shapes), nil
}

// compose appends non-colliding shapes directly beneath the shipped table's
// last row (never as a second table - a model matches the FIRST table it
// reads as the catalog, so a second one is invisible to routing). A shape
// whose trigger already matches an existing row (shipped or earlier custom,
// case/whitespace-insensitive) is refused with a warning naming it, rather
// than silently winning or losing a race with whichever the model reads
// first.
func compose(instructions string, shapes []Shape) string {
	lines := strings.Split(instructions, "\n")
	sepIdx := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == tableSep {
			sepIdx = i
			break
		}
	}
	if sepIdx == -1 {
		slog.Warn("workflow catalog: plan-work has no Common workflows table; custom shapes not composed",
			"component", "workflowcatalog")
		return instructions
	}

	end := sepIdx + 1
	seen := map[string]bool{}
	for end < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[end]), "|") {
		seen[normalize(rowTrigger(lines[end]))] = true
		end++
	}

	var newRows []string
	for _, s := range shapes {
		key := normalize(s.Trigger)
		if seen[key] {
			slog.Warn("workflow catalog: shape's trigger collides with an existing table row; shape skipped",
				"component", "workflowcatalog", "shape", s.Name, "trigger", s.Trigger)
			continue
		}
		seen[key] = true
		newRows = append(newRows, fmt.Sprintf("| %s | %s |", escapeCell(s.Trigger), escapeCell(s.DAGShape)))
	}
	if len(newRows) == 0 {
		return instructions
	}

	out := make([]string, 0, len(lines)+len(newRows))
	out = append(out, lines[:end]...)
	out = append(out, newRows...)
	out = append(out, lines[end:]...)
	return strings.Join(out, "\n")
}

// rowTrigger extracts a markdown table row's first cell.
func rowTrigger(row string) string {
	row = strings.TrimPrefix(strings.TrimSpace(row), "|")
	cell, _, _ := strings.Cut(row, "|")
	return strings.TrimSpace(cell)
}

func normalize(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// escapeCell keeps a shape's free text from breaking the row it's rendered into.
func escapeCell(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.TrimSpace(s)
}
