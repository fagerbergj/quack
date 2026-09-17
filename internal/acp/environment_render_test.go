package acp

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/workspace"
)

// badSource hands back one broken body for every name.
type badSource struct{ body string }

func (b badSource) Get(context.Context, string) (artifactsrc.Artifact, bool, error) {
	return artifactsrc.Artifact{Body: b.body, VersionID: "bad"}, true, nil
}
func (badSource) Seed(context.Context, string, artifactsrc.Artifact) error { return nil }

// hardErrSource always fails with artifactsrc.ErrHard - the one class of
// failure Resolve propagates instead of falling back to the shipped file.
type hardErrSource struct{}

func (hardErrSource) Get(context.Context, string) (artifactsrc.Artifact, bool, error) {
	return artifactsrc.Artifact{}, false, fmt.Errorf("pinned version gone: %w", artifactsrc.ErrHard)
}
func (hardErrSource) Seed(context.Context, string, artifactsrc.Artifact) error { return nil }

// TestEnvironmentBlockDegradesOnHardResolverError: a resolver that can't even
// fall back to the shipped file degrades the whole block to "", never panics
// or propagates the error into the round's prompt.
func TestEnvironmentBlockDegradesOnHardResolverError(t *testing.T) {
	res := artifactsrc.New("langfuse", hardErrSource{}, time.Minute)
	got, art := environmentBlock(context.Background(), res, t.TempDir(), workspace.Caps{})
	if got != "" || art.Name != "" {
		t.Errorf("environmentBlock = (%q, %+v), want (\"\", zero Artifact)", got, art)
	}
}

// TestEnvironmentBlockSurvivesBadStoredTemplate: a typo in a stored
// system/acp.environment falls back to the shipped file rather than silently
// dropping the block the round is grounded on.
func TestEnvironmentBlockSurvivesBadStoredTemplate(t *testing.T) {
	res := artifactsrc.New("langfuse", badSource{body: "{{if .Git}}unclosed"}, time.Minute)
	got, _ := environmentBlock(context.Background(), res, t.TempDir(), workspace.Caps{})
	if !strings.HasPrefix(got, "<environment_context>") || !strings.HasSuffix(got, "</environment_context>") {
		t.Errorf("block = %q, want the shipped template's output", got)
	}
}

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
		got, _, err := renderEnvironment(context.Background(), nil, c.f)
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
