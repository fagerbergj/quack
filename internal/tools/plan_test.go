package tools

import (
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/dag"
)

func TestNewPlanToolMetadata(t *testing.T) {
	// stubModel is defined in tools_test.go (same package).
	planner := dag.NewPlanner(stubModel{out: `{"nodes":[]}`}, nil)
	tl, err := NewPlanTool(planner, NewPlanCache())
	if err != nil {
		t.Fatalf("NewPlanTool error: %v", err)
	}
	if tl.Name() != "plan" {
		t.Errorf("Name() = %q, want %q", tl.Name(), "plan")
	}
	if !strings.Contains(tl.Description(), "DAG") {
		t.Errorf("Description() = %q, want mention of DAG", tl.Description())
	}
}
