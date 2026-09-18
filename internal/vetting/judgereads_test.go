package vetting

import "testing"

// The bug (#359): a judge scored an exploration answer 100% by rationalizing
// "the ledger shows they read exa.go" when the ledger was empty and the worker
// had web_fetched the file instead. A PASS earned without opening anything has verified nothing, so the verdict is discarded and re-judged once. A FAIL without reading is conservative, not dangerous, and is left alone.
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

// countReads must leave the judge's view of its tools unchanged - same names, so
// the model cannot tell it is being counted - and hadTools false when there are
// none to count.
func TestCountReadsIsTransparent(t *testing.T) {
	wrapped, c := countReads(nil)
	if len(wrapped) != 0 || c.hadTools {
		t.Errorf("countReads(nil) = %d tools, hadTools=%v; want 0, false", len(wrapped), c.hadTools)
	}
}

// TestUnreadArtifactPass (#1497): unlike unreadPass, this rule keys on
// whether the WORKER wrote an artifact this round, not on tool presence - a
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
