// judgereads.go: counts judge read-tool calls; a pass with zero reads is discarded.
// Only a PASS is checked (a fail without reads is conservative, not dangerous).
package vetting

import (
	"fmt"
	"strings"
	"sync/atomic"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/tool"
)

// readCounter: tally for one judge round's read-tool calls.
type readCounter struct {
	n        atomic.Int64
	hadTools bool // tool-less judge is never faulted
}

func (c *readCounter) count() int64 { return c.n.Load() }

// countReads tallies calls to these tools by name. A before-tool callback fires exactly when the
// tool would run, so the judge sees its tools unwrapped.
func countReads(tools []tool.Tool) (*readCounter, llmagent.BeforeToolCallback) {
	c := &readCounter{hadTools: len(tools) > 0}
	names := make(map[string]bool, len(tools))
	for _, t := range tools {
		names[t.Name()] = true
	}
	return c, func(_ agent.Context, t tool.Tool, _ map[string]any) (map[string]any, error) {
		if names[t.Name()] {
			c.n.Add(1)
		}
		return nil, nil
	}
}

// unreadPass: judge held read tools, passed, and never called one.
func unreadPass(c *readCounter, v verdict) bool {
	return c != nil && c.hadTools && v.Passed && c.count() == 0
}

// unreadPassFeedback: appended to judge prompt on re-run (states mechanism, not repeated instruction).
const unreadPassFeedback = "Your previous verdict passed this answer without opening a single file. " +
	"Read-tool calls are counted: a pass with zero reads is discarded, because nothing in it was verified " +
	"against the repository. Identify every specific claim the answer makes about this repo, check each one " +
	"with grep/glob/read_file, and score from what you actually found."

// judgeReadCounters: one round's repo-read and artifact-read tallies, kept
// separate so each zero-reads discard rule keys on its own tool set.
type judgeReadCounters struct {
	repo     *readCounter
	artifact *readCounter
	// reads: the round's tool results as "name(args) -> result", seeded into a retry's prompt.
	reads *[]string
}

// carry credits prior's reads to c: a retry seeded with them need not repeat them to pass.
func (c judgeReadCounters) carry(prior judgeReadCounters) {
	for _, p := range [][2]*readCounter{{c.repo, prior.repo}, {c.artifact, prior.artifact}} {
		if p[0] != nil && p[1] != nil {
			p[0].n.Add(p[1].count())
		}
	}
}

// judgePriorReadsChars bounds the prior-attempt reads seeded into a retry's prompt.
const judgePriorReadsChars = 32_000

// priorReadsSection renders c's reads for a retry of the same answer within limit chars;
// "" when there are none or no room.
func priorReadsSection(c judgeReadCounters, limit int) string {
	const header = "READS FROM YOUR PREVIOUS ATTEMPT on this same answer - reuse them instead of repeating them; a read marked \"excerpt truncated\" may be read again for the part it cut:\n"
	if c.reads == nil || len(*c.reads) == 0 || limit < len(header)+200 {
		return ""
	}
	var b strings.Builder
	b.WriteString(header)
	skipped := 0
	for _, r := range *c.reads {
		if b.Len()+len(r)+60 > limit { // 60: room for the skipped-reads note
			skipped++
			continue
		}
		b.WriteString(r)
		b.WriteString("\n")
	}
	if skipped > 0 {
		fmt.Fprintf(&b, "(%d reads not shown: no room)\n", skipped)
	}
	return b.String()
}

// unreadArtifactPass: the worker wrote an artifact this round and the judge passed without calling
// read_artifact, so nothing checked what the worker produced.
func unreadArtifactPass(c *readCounter, v verdict, workerWroteArtifact bool) bool {
	return c != nil && workerWroteArtifact && v.Passed && c.count() == 0
}

// unreadArtifactPassFeedback: appended to the judge prompt on re-run, naming
// the exact ids so the retry has no excuse to skip them.
func unreadArtifactPassFeedback(ids []string) string {
	return "Your previous verdict passed this answer, but the worker wrote or edited an artifact this round " +
		"and you never called read_artifact. Read it before judging: " + strings.Join(ids, ", ") + ". " +
		"If the answer refers to an artifact instead of containing the deliverable, the artifact's content IS the answer."
}
