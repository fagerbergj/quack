package acp

import (
	"context"
	"strings"
	"testing"
)

// TestRenderEnvironmentBranches covers the system/acp.environment branches the
// golden files cannot reach (a git repo, a truncated entry list) - the template
// is what renders them now, so a broken conditional must fail here.
func TestRenderEnvironmentBranches(t *testing.T) {
	base := envFacts{Cwd: "/w", Entries: "a, b", MaxEntries: 200}
	for _, c := range []struct {
		name string
		f    envFacts
		want string
	}{
		{"git with head", func() envFacts {
			f := base
			f.Git, f.Branch, f.Sha = true, "main", "deadbee"
			return f
		}(), "git: yes (branch main, HEAD deadbee)"},
		{"git without head", func() envFacts {
			f := base
			f.Git, f.Branch = true, "main"
			return f
		}(), "git: yes (branch main)"},
		{"truncated entries", func() envFacts {
			f := base
			f.Truncated = true
			return f
		}(), "entries (first 200): a, b"},
	} {
		got, err := renderEnvironment(context.Background(), nil, c.f)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !strings.Contains(got, c.want+"\n") {
			t.Errorf("%s: %q missing line %q", c.name, got, c.want)
		}
		if !strings.HasSuffix(got, "</environment_context>") {
			t.Errorf("%s: block does not end at the closing tag: %q", c.name, got)
		}
	}
}
