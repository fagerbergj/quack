package agent

import "testing"

// The provider's fixed overhead (system instruction + ~20 tool schemas) must be
// modelled ADDITIVELY, never as a multiplier.
//
// The live failure: a code-explorer node's first turn carried a 728-character task
// (~200 tokens estimated) while the provider billed ~6000 prompt tokens for the
// system instruction and tool schemas. The old model learned measured/estimate ≈ 30,
// clamped to the 8.0 ceiling, and then multiplied EVERY later turn by 8 - declaring a
// genuinely ~7k-token request to be 56,344 tokens, roughly 49k of which never existed.
// Compaction then found nothing it could free on a fresh session and shredded
// contents[0]. The worker lost its task and woke up asking:
//
//	"Which repository would you like me to explore?"
func TestCalibrationDoesNotInflateSmallRequests(t *testing.T) {
	const density = defaultCalibrationRatio

	// A node's first turn: tiny content, large fixed overhead.
	firstEstimate := 200  // the 728-char task
	firstMeasured := 6200 // provider bills the system instruction + tool schemas too

	// What recordUsage now learns: the overhead, not a multiplier.
	overhead := firstMeasured - int(float64(firstEstimate)*density)
	if overhead < 0 {
		overhead = 0
	}

	// A LATER turn whose real content is ~7k tokens.
	laterEstimate := 7043
	got := calibrated(laterEstimate, density, overhead)

	// Truth: overhead + density*content ≈ 6200-260 + 9155 ≈ 15k. Nowhere near 56k.
	want := overhead + int(float64(laterEstimate)*density)
	if got != want {
		t.Fatalf("calibrated = %d, want %d", got, want)
	}
	if got > 20_000 {
		t.Fatalf("calibrated a ~7k-token request at %d tokens - the fixed overhead is being multiplied again", got)
	}

	// And the old model's fiction, for contrast: it must not be reachable.
	oldModel := int(float64(laterEstimate) * 8.0) // ratio pinned at the ceiling
	if got >= oldModel {
		t.Fatalf("calibrated (%d) is no better than the old multiplicative model (%d)", got, oldModel)
	}

	// The budget on the live 65k window is ~45k. The old model blew past it on a
	// request that comfortably fits; the new one must not.
	const budget = 45_536
	if got > budget {
		t.Fatalf("a ~7k-token request calibrated to %d, over the %d budget - compaction would panic and shred the task",
			got, budget)
	}
}

// The overhead must never be negative (a turn the provider tokenises more efficiently
// than our estimate must not produce a negative correction).
func TestOverheadNeverNegative(t *testing.T) {
	measured, est := 100, 10_000 // absurdly efficient tokenisation
	overhead := measured - int(float64(est)*defaultCalibrationRatio)
	if overhead < 0 {
		overhead = 0
	}
	if got := calibrated(est, defaultCalibrationRatio, overhead); got <= 0 {
		t.Fatalf("calibrated = %d; must stay positive", got)
	}
}
