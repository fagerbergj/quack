package replay

import "fmt"

// Class classifies one structural divergence between a live call and the
// recording (.quack/replay-log.md "Replay semantics"). Payload bytes are
// never matched - only sequence position + shallow identity (model/tool
// NAME) - so these are the only three ways a live call can fail to line up
// with the recording.
type Class string

const (
	// ClassExtra: the live call landed on a stream position the recording
	// never reached (the stream is exhausted, or was never recorded at all).
	ClassExtra Class = "extra"
	// ClassMismatched: the live call's identity (model/tool name) disagrees
	// with the recorded entry at that exact sequence position.
	ClassMismatched Class = "mismatched"
)

// NearMiss is one recorded entry near a divergence, and which identity
// field disagreed - vcrpy-style, so a MissError never reads as a bare "not
// found".
type NearMiss struct {
	Position int    // sequence position within the stream's op-kind subsequence
	Name     string // the recorded model/tool name at this position
	Field    string // which identity field this compares ("model" or "tool")
}

// MissError is returned by Session.NextChat/NextToolResult on any
// structural divergence. replay-strict treats it as fatal: the caller (the
// replay-backed model or tool stub) surfaces it as the call's own error, so
// the run fails loudly at the exact point of divergence.
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

// PromptDrift is an INFORMATIONAL divergence: the live system instruction's
// content hash disagrees with the recorded gen_ai.prompt.version at the same
// sequence position. Never fails a replay - only structural divergence does.
type PromptDrift struct {
	Stream   StreamKey
	Position int
	Recorded string
	Live     string
}

// StreamReport is one stream's consumption tally at Report() time.
// Consumed < Total means the live run never made every call the recording
// did (a shorter run, or a genuinely missing call) - surfaced here rather
// than failed loudly, since nothing live actively contradicted the
// recording.
type StreamReport struct {
	Stream   StreamKey
	Op       string // "chat", or a tool name
	Consumed int
	Total    int
}

// Report is a session's full divergence accounting: per-stream consumption,
// informational prompt drift, and every structural MissError encountered
// along the way (in the order returned to callers, thread-safe accumulation
// via Session's mutex).
type Report struct {
	Streams  []StreamReport
	Drift    []PromptDrift
	Failures []*MissError
}

// Clean reports whether nothing in r indicates any divergence at all -
// every stream fully consumed, no prompt drift, no structural failures.
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
