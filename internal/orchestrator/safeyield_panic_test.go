package orchestrator

import (
	"iter"
	"testing"

	"github.com/fagerbergj/quack/internal/stream"
)

// #1033: newSafeYield used to recover a consumer's loop-body panic and return
// normally. Go then panics at the RANGE SITE with "range function recovered a
// loop body panic and did not resume panicking" - which in production lands in
// startRun's detached goroutine and kills the process. The consumer must see
// its OWN panic value instead.
func TestSafeYield_ResumesConsumerPanic(t *testing.T) {
	seq := iter.Seq2[stream.SSEEvent, error](func(yield func(stream.SSEEvent, error) bool) {
		newSafeYield(yield)(stream.Done(), nil)
	})

	var got any
	func() {
		defer func() { got = recover() }()
		for range seq {
			panic("boom in loop body")
		}
	}()

	if got == nil {
		t.Fatal("consumer panic was swallowed entirely")
	}
	if s, _ := got.(string); s != "boom in loop body" {
		t.Fatalf("consumer must observe its own panic value, got %v (%T)", got, got)
	}
}

// A consumer that breaks out of the range makes yield return false - the
// ordinary dropped-SSE-client case. Re-entering that exhausted closure is
// itself a panic, so safeYield must latch stopped on a false return (#1033).
func TestSafeYield_StopsAfterConsumerBreaks(t *testing.T) {
	var after bool
	var panicked any

	seq := iter.Seq2[stream.SSEEvent, error](func(yield func(stream.SSEEvent, error) bool) {
		sy := newSafeYield(yield)
		sy(stream.Done(), nil) // consumer breaks during this call
		func() {
			defer func() { panicked = recover() }()
			after = sy(stream.Done(), nil)
		}()
	})

	for range seq {
		break
	}

	if panicked != nil {
		t.Fatalf("re-entry after consumer break panicked: %v", panicked)
	}
	if after {
		t.Fatal("safeYield must report false once the consumer has stopped ranging")
	}
}
