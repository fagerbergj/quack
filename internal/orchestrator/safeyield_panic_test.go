package orchestrator

import (
	"iter"
	"testing"

	"github.com/fagerbergj/quack/internal/stream"
)

// A recovered loop-body panic makes Go panic at the range site and kill the process, so the
// consumer must see its own panic value.
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

// A consumer break makes yield return false; re-entering it panics, so safeYield must latch stopped.
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
