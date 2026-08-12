package sdk

import "testing"

func TestRegistered(t *testing.T) {
	// The registry starts empty; tests that call Register should clean up.
	if len(Registered()) != 0 {
		t.Errorf("expected empty registry, got %d entries", len(Registered()))
	}
}
