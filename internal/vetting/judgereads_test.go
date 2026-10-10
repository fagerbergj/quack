package vetting

import (
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

// runnableTool: what a functiontool-built tool.Tool satisfies.
type runnableTool interface {
	tool.Tool
	Declaration() *genai.FunctionDeclaration
	Run(ctx agent.Context, args any) (map[string]any, error)
}

type namedTool string

func (n namedTool) Name() string      { return string(n) }
func (namedTool) Description() string { return "" }
func (namedTool) IsLongRunning() bool { return false }

// A PASS earned without opening anything verified nothing and is re-judged once; a FAIL without
// reading is conservative and left alone.
func TestUnreadPass(t *testing.T) {
	withTools := func(reads int64) *readCounter {
		c := &readCounter{hadTools: true}
		c.n.Store(reads)
		return c
	}
	for _, tc := range []struct {
		name string
		c    *readCounter
		v    verdict
		want bool
	}{
		{"passed without reading", withTools(0), verdict{Passed: true}, true},
		{"passed after reading", withTools(3), verdict{Passed: true}, false},
		{"failed without reading", withTools(0), verdict{Passed: false}, false},
		{"tool-less judge cannot read", &readCounter{hadTools: false}, verdict{Passed: true}, false},
		{"no counter", nil, verdict{Passed: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := unreadPass(tc.c, tc.v); got != tc.want {
				t.Errorf("unreadPass = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCountReadsCountsOnlyItsTools(t *testing.T) {
	read := namedTool("read_file")
	c, before := countReads([]tool.Tool{read})
	if !c.hadTools {
		t.Fatal("hadTools = false with a read tool")
	}
	_, _ = before(nil, read, nil)
	_, _ = before(nil, namedTool("submit_verdict"), nil)
	if c.count() != 1 {
		t.Errorf("count = %d, want 1 (only read_file)", c.count())
	}
	if c, _ := countReads(nil); c.hadTools {
		t.Error("countReads(nil): hadTools = true")
	}
}

// TestUnreadArtifactPass keys on whether the worker wrote an artifact, not on tool presence: a
// research node with the tools but no write is never faulted.
func TestUnreadArtifactPass(t *testing.T) {
	withReads := func(reads int64) *readCounter {
		c := &readCounter{}
		c.n.Store(reads)
		return c
	}
	for _, tc := range []struct {
		name  string
		c     *readCounter
		v     verdict
		wrote bool
		want  bool
	}{
		{"passed with zero reads, worker wrote", withReads(0), verdict{Passed: true}, true, true},
		{"passed after reading, worker wrote", withReads(1), verdict{Passed: true}, true, false},
		{"failed with zero reads, worker wrote", withReads(0), verdict{Passed: false}, true, false},
		{"passed with zero reads, worker wrote nothing", withReads(0), verdict{Passed: true}, false, false},
		{"no counter", nil, verdict{Passed: true}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := unreadArtifactPass(tc.c, tc.v, tc.wrote); got != tc.want {
				t.Errorf("unreadArtifactPass = %v, want %v", got, tc.want)
			}
		})
	}
}
