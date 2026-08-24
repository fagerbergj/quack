package dag

import (
	"context"
	"testing"
	"time"
)

// #1038: the aging gate is global. Once ANY waiter ages past the threshold,
// fits() is false for every other waiter regardless of what it wants - so a
// node contending for nothing stalls behind an unrelated starved one while its
// own capacity sits idle. On the deployed config (two models with session caps,
// node runtimes in minutes against a 2-minute threshold) that presents as a DAG
// hanging, which is easy to misread as a deadlock.
func TestAdmit_AgedWaiterDoesNotBlockAnUnrelatedDimension(t *testing.T) {
	const aging = 40 * time.Millisecond
	a := NewAdmission(map[string]int{"busy": 1, "free": 4}, nil, nil, aging)

	busy := AdmissionSpec{Model: "busy"}
	free := AdmissionSpec{Model: "free"}

	// Holder occupies the single "busy" slot.
	if !a.Admit(context.Background(), busy, nil) {
		t.Fatal("holder should be admitted")
	}

	// A second "busy" waiter blocks and is left to age past the threshold.
	blocked := make(chan bool, 1)
	go func() { blocked <- a.Admit(context.Background(), busy, nil) }()
	time.Sleep(aging * 3)

	// A "free" node contends for nothing the aged waiter holds: three of four
	// slots are idle. It must be admitted immediately.
	done := make(chan bool, 1)
	go func() { done <- a.Admit(context.Background(), free, nil) }()

	select {
	case ok := <-done:
		if !ok {
			t.Fatal("unrelated free-dimension node was refused")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("unrelated node stalled behind an aged waiter on a different model; 3 of 4 slots were idle")
	}

	a.Release(busy) // let the aged waiter through so the goroutine exits
	<-blocked
}

// The anti-starvation guarantee must survive: an aged waiter still blocks
// backfill by a LATER waiter that contends for the same resource.
func TestAdmit_AgedWaiterStillBlocksItsOwnDimension(t *testing.T) {
	const aging = 40 * time.Millisecond
	a := NewAdmission(map[string]int{"busy": 1}, nil, nil, aging)
	busy := AdmissionSpec{Model: "busy"}

	if !a.Admit(context.Background(), busy, nil) {
		t.Fatal("holder should be admitted")
	}
	aged := make(chan bool, 1)
	go func() { aged <- a.Admit(context.Background(), busy, nil) }()
	time.Sleep(aging * 3)

	// A latecomer on the SAME model must not jump the aged waiter.
	late := make(chan bool, 1)
	go func() { late <- a.Admit(context.Background(), busy, nil) }()
	time.Sleep(aging)

	a.Release(busy) // exactly one slot frees: it must go to the aged waiter
	select {
	case <-aged:
	case <-late:
		t.Fatal("a latecomer backfilled past an aged waiter on the same model")
	case <-time.After(2 * time.Second):
		t.Fatal("neither waiter was admitted")
	}
	a.Release(busy)
	<-late
}
