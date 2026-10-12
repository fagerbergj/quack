package sdk_test

import (
	"context"
	"testing"

	"github.com/fagerbergj/quack-extensions/sdk"
)

// fakeAssignmentExt pins AssignmentMetaExtension/AssignmentFreshnessChecker's
// signatures the same way TestHostContextDirChatUserArchiveChat pins Host's -
// a real extension implements these via structural typing, detected by quack
// with a type assertion, so the sdk package only proves the shape is callable.
type fakeAssignmentExt struct {
	meta   map[string]any
	fresh  bool
	reason string
}

func (f fakeAssignmentExt) OnAssignment(context.Context, sdk.Assignment) map[string]any {
	return f.meta
}

func (f fakeAssignmentExt) BeforeAssignment(context.Context, sdk.Assignment) (bool, string) {
	return f.fresh, f.reason
}

func TestAssignmentMetaExtensionDetection(t *testing.T) {
	var ext any = fakeAssignmentExt{meta: map[string]any{"base_sha": "deadbeef"}}
	m, ok := ext.(sdk.AssignmentMetaExtension)
	if !ok {
		t.Fatal("want fakeAssignmentExt detected via its optional OnAssignment method")
	}
	a := sdk.Assignment{PlanID: "p1", NodeID: "impl-1", Agent: "code-implementer", Task: "keep going"}
	got := m.OnAssignment(context.Background(), a)
	if got["base_sha"] != "deadbeef" {
		t.Errorf("OnAssignment = %+v, want base_sha forwarded", got)
	}
}

func TestAssignmentFreshnessCheckerDetection(t *testing.T) {
	var ext any = fakeAssignmentExt{fresh: false, reason: "base moved: abc1234 -> def5678"}
	c, ok := ext.(sdk.AssignmentFreshnessChecker)
	if !ok {
		t.Fatal("want fakeAssignmentExt detected via its optional BeforeAssignment method")
	}
	fresh, reason := c.BeforeAssignment(context.Background(), sdk.Assignment{NodeID: "impl-1"})
	if fresh || reason == "" {
		t.Errorf("BeforeAssignment = (%v, %q), want (false, a non-empty reason)", fresh, reason)
	}
}
