package dag

import "testing"

func TestCanTransition(t *testing.T) {
	tests := []struct {
		name string
		from NodeStatus
		to   NodeStatus
		want bool
	}{
		// legal edges
		{"queued to running", StatusQueued, StatusRunning, true},
		{"queued to cancelled", StatusQueued, StatusCancelled, true},
		{"queued to failed (stale on restart)", StatusQueued, StatusFailed, true},
		{"running to paused", StatusRunning, StatusPaused, true},
		{"running to needs_input", StatusRunning, StatusNeedsInput, true},
		{"running to done", StatusRunning, StatusDone, true},
		{"running to failed", StatusRunning, StatusFailed, true},
		{"running to cancelled", StatusRunning, StatusCancelled, true},
		{"paused to running (resume)", StatusPaused, StatusRunning, true},
		{"paused to cancelled", StatusPaused, StatusCancelled, true},
		{"needs_input to running (resumed)", StatusNeedsInput, StatusRunning, true},
		{"needs_input to cancelled", StatusNeedsInput, StatusCancelled, true},
		{"done to queued (retry)", StatusDone, StatusQueued, true},
		{"failed to queued (retry)", StatusFailed, StatusQueued, true},
		{"cancelled to queued (retry)", StatusCancelled, StatusQueued, true},
		{"empty from defaults to queued", "", StatusRunning, true},

		// illegal edges
		{"running to running (no more self-loop steer)", StatusRunning, StatusRunning, false},
		{"done to running", StatusDone, StatusRunning, false},
		{"cancelled to needs_input", StatusCancelled, StatusNeedsInput, false},
		{"failed to running", StatusFailed, StatusRunning, false},
		{"failed to done", StatusFailed, StatusDone, false},
		{"done to cancelled", StatusDone, StatusCancelled, false},
		{"done to failed", StatusDone, StatusFailed, false},
		{"queued to done", StatusQueued, StatusDone, false},
		{"queued to needs_input", StatusQueued, StatusNeedsInput, false},
		{"needs_input to done", StatusNeedsInput, StatusDone, false},
		{"needs_input to failed", StatusNeedsInput, StatusFailed, false},
		{"cancelled to running", StatusCancelled, StatusRunning, false},
		{"cancelled to done", StatusCancelled, StatusDone, false},
		{"paused to done", StatusPaused, StatusDone, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CanTransition(tt.from, tt.to); got != tt.want {
				t.Errorf("CanTransition(%q, %q) = %v, want %v", tt.from, tt.to, got, tt.want)
			}
		})
	}
}

func TestAllowedTargets(t *testing.T) {
	tests := []struct {
		name string
		from NodeStatus
		want []NodeStatus
	}{
		{"queued", StatusQueued, []NodeStatus{StatusCancelled, StatusFailed, StatusQueued, StatusRunning}},
		{"running", StatusRunning, []NodeStatus{StatusCancelled, StatusDone, StatusFailed, StatusNeedsInput, StatusPaused}},
		{"paused", StatusPaused, []NodeStatus{StatusCancelled, StatusRunning}},
		{"needs_input", StatusNeedsInput, []NodeStatus{StatusCancelled, StatusRunning}},
		{"done", StatusDone, []NodeStatus{StatusQueued}},
		{"failed", StatusFailed, []NodeStatus{StatusQueued}},
		{"cancelled", StatusCancelled, []NodeStatus{StatusQueued}},
		{"empty defaults to queued", "", []NodeStatus{StatusCancelled, StatusFailed, StatusQueued, StatusRunning}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AllowedTargets(tt.from)
			if len(got) != len(tt.want) {
				t.Fatalf("AllowedTargets(%q) = %v, want %v", tt.from, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("AllowedTargets(%q)[%d] = %v, want %v", tt.from, i, got[i], tt.want[i])
				}
			}
		})
	}
}
