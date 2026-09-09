package store

import (
	"fmt"
	"iter"
	"testing"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// mkEvents builds nTurns turns of perTurn orchestrator events, each carrying chunk text -
// shaped after production streaming, where one turn arrives as many small text chunks.
func mkEvents(nTurns, perTurn int, chunk string) []*session.Event {
	var out []*session.Event
	for t := 0; t < nTurns; t++ {
		ue := &session.Event{Author: "user"}
		ue.Content = &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "please do the thing"}}}
		out = append(out, ue)
		for k := 0; k < perTurn; k++ {
			oe := &session.Event{Author: orchestratorAuthor}
			oe.Content = &genai.Content{Role: "model", Parts: []*genai.Part{{Text: chunk}}}
			out = append(out, oe)
		}
	}
	return out
}

func seqOf(evs []*session.Event) iter.Seq[*session.Event] {
	return func(yield func(*session.Event) bool) {
		for _, e := range evs {
			if !yield(e) {
				return
			}
		}
	}
}

// BenchmarkGroupSessionEvents pins perf audit #4: groupSessionEvents' turn text fields
// must stay strings.Builder, not +=, or a 2,000-event turn goes back to 197 MB/op.
func BenchmarkGroupSessionEvents(b *testing.B) {
	chunk := "a chunk of assistant output text that is roughly the size of one streamed segment in production"
	for _, n := range []struct{ turns, per int }{{50, 280}, {10, 280}, {1, 280}, {1, 2000}, {50, 1000}} {
		evs := mkEvents(n.turns, n.per, chunk)
		b.Run(fmt.Sprintf("turns%d_per%d", n.turns, n.per), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				g := groupSessionEvents(seqOf(evs))
				if len(g) != n.turns {
					b.Fatal(len(g))
				}
			}
		})
	}
}
