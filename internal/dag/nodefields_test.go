package dag

import (
	"strings"
	"testing"
)

func TestValidateAgentNameAcceptsRosterMember(t *testing.T) {
	SetAgentRoster([]AgentInfo{{Name: "code-implementer"}, {Name: "code-reviewer"}})
	if err := ValidateAgentName("code-reviewer"); err != nil {
		t.Fatalf("ValidateAgentName: %v", err)
	}
}

func TestValidateAgentNameRejectsUnknown(t *testing.T) {
	SetAgentRoster([]AgentInfo{{Name: "code-implementer"}})
	err := ValidateAgentName("cod-implementer")
	if err == nil {
		t.Fatal("want an error for an unknown agent")
	}
	if !strings.HasPrefix(err.Error(), "agent:") {
		t.Errorf("error = %q, want it to name the field", err)
	}
	if !strings.Contains(err.Error(), "did you mean") {
		t.Errorf("error = %q, want a near-match suggestion", err)
	}
}

func TestValidateAgentNameRejectsEmpty(t *testing.T) {
	if err := ValidateAgentName(""); err == nil {
		t.Fatal("want an error for an empty agent")
	}
}

func TestValidateTaskRejectsBlank(t *testing.T) {
	if err := ValidateTask("   "); err == nil {
		t.Fatal("want an error for a blank task")
	}
	if err := ValidateTask("do the thing"); err != nil {
		t.Fatalf("ValidateTask: %v", err)
	}
}

func TestNewPlannerSyncsAgentRoster(t *testing.T) {
	NewPlanner([]AgentInfo{{Name: "web-researcher"}}, nil, nil)
	if err := ValidateAgentName("web-researcher"); err != nil {
		t.Fatalf("NewPlanner must sync the package-level roster: %v", err)
	}
}

func TestValidateWorkdir(t *testing.T) {
	cases := []struct {
		workdir string
		wantErr bool
	}{
		{"", false},
		{"repo", false},
		{"repo/sub", false},
		{"/etc/passwd", true},
		{"../escape", true},
		{"repo/../../escape", true},
	}
	for _, c := range cases {
		err := ValidateWorkdir(c.workdir)
		if (err != nil) != c.wantErr {
			t.Errorf("ValidateWorkdir(%q) = %v, wantErr %v", c.workdir, err, c.wantErr)
		}
	}
}
