package replay

import "fmt"

// Class classifies one structural divergence. Payload bytes are never matched;
// only sequence position + shallow identity (model/tool name).
type Class string

const (
	ClassExtra      Class = "extra"      // recording exhausted or never existed
	ClassMismatched Class = "mismatched" // identity disagrees at this position
)

// NearMiss is one recorded entry near a divergence, vcrpy-style.
type NearMiss struct {
	Position int    // sequence position within the stream's op-kind subsequence
	Name     string // the recorded model/tool name at this position
	Field    string // which identity field this compares ("model" or "tool")
}

// MissError is returned on any structural divergence. Fatal in strict mode.
type MissError struct {
	Class    Class
	Stream   StreamKey
	Op       string // "chat", or the tool name for an execute_tool miss
	Position int    // sequence position the live call landed on
	Want     string // the identity (model/tool name) the live call presented
	Diff     []NearMiss
}

func (e *MissError) Error() string {
	switch e.Class {
	case ClassMismatched:
		return fmt.Sprintf("replay: mismatched %s call in stream %s at position %d - recorded %q, live wants %q",
			e.Op, e.Stream, e.Position, e.Diff[0].Name, e.Want)
	default:
		if len(e.Diff) == 0 {
			return fmt.Sprintf("replay: extra %s call in stream %s at position %d (%q) - nothing was recorded for this stream",
				e.Op, e.Stream, e.Position, e.Want)
		}
		return fmt.Sprintf("replay: extra %s call in stream %s at position %d (%q) - nearest recorded: %v",
			e.Op, e.Stream, e.Position, e.Want, e.Diff)
	}
}

// ForkSignal is returned instead of MissError when fork mode goes live.
// Sticky: every later call in the same stream returns "sticky".
type ForkSignal struct {
	Stream StreamKey
	Reason string     // "fork-from" (explicit --fork-from boundary), "miss" (structural divergence), or "sticky" (already forked)
	Cause  *MissError // set when Reason == "miss"; nil otherwise
}

func (f *ForkSignal) Error() string {
	if f.Cause != nil {
		return fmt.Sprintf("replay: fork-replay: stream %s went live (%v)", f.Stream, f.Cause)
	}
	return fmt.Sprintf("replay: fork-replay: stream %s went live (%s)", f.Stream, f.Reason)
}

// PromptDrift is informational: live system instruction hash disagrees with recorded version.
type PromptDrift struct {
	Stream   StreamKey
	Position int
	Recorded string
	Live     string
}

// StreamReport is one stream's consumption tally. Consumed < Total means a shorter live run.
type StreamReport struct {
	Stream   StreamKey
	Op       string // "chat", or a tool name
	Consumed int
	Total    int
}

// Report is a session's full divergence accounting.
type Report struct {
	Streams  []StreamReport
	Drift    []PromptDrift
	Failures []*MissError
	// Streams that switched to live in fork mode (informational).
	Forked []*ForkSignal
}

// Clean reports whether the replay is divergence-free.
func (r Report) Clean() bool {
	if len(r.Drift) != 0 || len(r.Failures) != 0 {
		return false
	}
	for _, s := range r.Streams {
		if s.Consumed != s.Total {
			return false
		}
	}
	return true
}
