// Package workflowcatalog composes deployment-defined DAG shapes into plan-work's "Common workflows" table
// at startup, so an operator can teach the planner a shape without forking the skill.
package workflowcatalog

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"

	"google.golang.org/adk/v2/tool/skilltoolset/skill"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/skillsource"
)

// planWorkSkill is the only skill this package augments.
const planWorkSkill = "plan-work"

// tableSep anchors the Common workflows table; every contiguous "|" line below it, up to a blank line, is
// a row.
const tableSep = "| --- | --- |"

// Shape is a composed catalog entry with provenance. Source is always "operator" and Approved always true:
// quack.yaml is the only source, and being in it is the approval.
type Shape struct {
	Name     string
	Trigger  string
	DAGShape string
	Agents   []string
	Source   string
	Version  string
	Approved bool
	// Nodes is non-empty only for a bound shape, which Bind renders into a dag.Plan without the planner;
	// empty means the shape is a planner hint only.
	Nodes []config.WorkflowNode
}

// FromConfig maps config entries to Shapes; validateWorkflows has already dropped malformed ones.
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

// DropAgents removes any shape naming an agent in dropped (in Agents or a bound node), one warning each.
func DropAgents(shapes []Shape, dropped map[string]bool) []Shape {
	if len(dropped) == 0 {
		return shapes
	}
	out := make([]Shape, 0, len(shapes))
	for _, s := range shapes {
		if agent, ok := shapeDroppedAgent(s, dropped); ok {
			slog.Warn("workflow catalog: shape names a dropped optional agent; shape removed",
				"component", "workflowcatalog", "shape", s.Name, "agent", agent)
			continue
		}
		out = append(out, s)
	}
	return out
}

// shapeDroppedAgent returns the first agent of s (its own list, then each
// bound node) that dropped names, if any.
func shapeDroppedAgent(s Shape, dropped map[string]bool) (string, bool) {
	for _, a := range s.Agents {
		if dropped[a] {
			return a, true
		}
	}
	for _, n := range s.Nodes {
		if dropped[n.Agent] {
			return n.Agent, true
		}
	}
	return "", false
}

// askPlaceholder is the only substitution a bound node's task supports, deliberately no template engine.
const askPlaceholder = "{{ask}}"

// Bind renders shape's bound nodes, replacing {{ask}} in each task. ok is false for a shape with no bound
// nodes.
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

// WrapRef appends shapesRef's current value to plan-work's table on every
// LoadInstructions call - re-read live, since shapesRef can still change.
func WrapRef(src skill.Source, shapesRef *atomic.Pointer[[]Shape]) skill.Source {
	return &augmentedRef{Source: src, shapesRef: shapesRef}
}

type augmentedRef struct {
	skill.Source
	shapesRef *atomic.Pointer[[]Shape]
}

func (a *augmentedRef) LoadInstructions(ctx context.Context, name string) (string, error) {
	var shapes []Shape
	if a.shapesRef != nil { // tests build sources with no ref
		if p := a.shapesRef.Load(); p != nil {
			shapes = *p
		}
	}
	if len(shapes) == 0 {
		// Skip compose entirely - otherwise a shapeless deployment logs its
		// "no Common workflows table" warning on every single load.
		return a.Source.LoadInstructions(ctx, name)
	}
	instructions, err := a.Source.LoadInstructions(ctx, name)
	// plan-work is now plugin-qualified ("quack:plan-work", #1427 S2) - match
	// by bare name so composition survives the prefix.
	if err != nil || skillsource.BareName(name) != planWorkSkill {
		return instructions, err
	}
	return compose(instructions, shapes), nil
}

// compose appends non-colliding shapes beneath the shipped table (a model only reads the first table). A
// trigger matching an existing row (case/space-insensitive) is refused with a warning.
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
