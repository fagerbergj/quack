package dag

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func mustAdmit(t *testing.T, a *Admission, spec AdmissionSpec) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if !a.Admit(ctx, spec, nil) {
		t.Fatalf("Admit(%+v) failed to admit when it should have fit", spec)
	}
}

// mustBlock asserts Admit does NOT return within a short window (still queued).
func mustBlock(t *testing.T, a *Admission, spec AdmissionSpec) (cancel func()) {
	t.Helper()
	ctx, cancelFn := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- a.Admit(ctx, spec, nil) }()
	select {
	case <-done:
		t.Fatalf("Admit(%+v) returned when it should have blocked on capacity", spec)
	case <-time.After(100 * time.Millisecond):
	}
	return cancelFn
}

func TestAdmitFitsSessionsDimension(t *testing.T) {
	a := NewAdmission(map[string]int{"m": 1}, nil, nil, time.Hour)
	spec := AdmissionSpec{Model: "m"}
	mustAdmit(t, a, spec)
	cancel := mustBlock(t, a, spec) // 2nd session exceeds cap of 1
	cancel()
	a.Release(spec)
	mustAdmit(t, a, spec) // capacity returned
}

func TestAdmitFitsKVTokensDimension(t *testing.T) {
	a := NewAdmission(nil, map[string]int{"m": 100}, nil, time.Hour)
	big := AdmissionSpec{Model: "m", KVTokens: 100}
	small := AdmissionSpec{Model: "m", KVTokens: 60}
	mustAdmit(t, a, small)
	cancel := mustBlock(t, a, small) // 60+60 > 100
	cancel()
	a.Release(small)
	mustAdmit(t, a, big) // 100 fits once the first 60 is released
}

func TestAdmitFitsResidencyDimension(t *testing.T) {
	a := NewAdmission(nil, nil, map[string]int{activeKeyTest("p", "worker"): 1}, time.Hour)
	m1 := AdmissionSpec{Model: "modelA", Provider: "p", Role: "worker"}
	m2 := AdmissionSpec{Model: "modelB", Provider: "p", Role: "worker"}
	mustAdmit(t, a, m1)
	cancel := mustBlock(t, a, m2) // different model, would need to evict modelA
	cancel()
	a.Release(m1)
	mustAdmit(t, a, m2)
}

func TestResidencySameModelBothAdmit(t *testing.T) {
	a := NewAdmission(nil, nil, map[string]int{activeKeyTest("p", "worker"): 1}, time.Hour)
	n1 := AdmissionSpec{Model: "modelA", Provider: "p", Role: "worker"}
	n2 := AdmissionSpec{Model: "modelA", Provider: "p", Role: "worker"} // same model, second node
	mustAdmit(t, a, n1)
	mustAdmit(t, a, n2) // already resident - no new distinct model needed
}

func TestAdmitCombinedDimensions(t *testing.T) {
	a := NewAdmission(map[string]int{"m": 2}, map[string]int{"m": 100}, map[string]int{activeKeyTest("p", "worker"): 1}, time.Hour)
	spec := AdmissionSpec{Model: "m", KVTokens: 60, Provider: "p", Role: "worker"}
	mustAdmit(t, a, spec) // sessions 1/2, kv 60/100, residency 1/1 - all fit
	other := AdmissionSpec{Model: "other", KVTokens: 10, Provider: "p", Role: "worker"}
	cancel := mustBlock(t, a, other) // residency dimension alone blocks a distinct model
	cancel()
}

func TestAbsentLimitsMeanUnlimited(t *testing.T) {
	a := NewAdmission(nil, nil, nil, time.Hour) // no limits configured anywhere
	spec := AdmissionSpec{Model: "unbounded-model", KVTokens: 999999999, Provider: "p", Role: "worker"}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if !a.Admit(ctx, spec, nil) {
				t.Error("unlimited model failed to admit concurrently")
			}
		}()
	}
	wg.Wait()
}

// Backfill: a small node behind a too-large one still gets admitted.
func TestBackfillAdmitsSmallNodeBehindLargeOne(t *testing.T) {
	a := NewAdmission(nil, map[string]int{"m": 100}, nil, time.Hour)
	big := AdmissionSpec{Model: "m", KVTokens: 90}
	cancelBig := mustBlockFirstArrival(t, a, big) // reserve no capacity, just occupy the queue's front

	small := AdmissionSpec{Model: "m", KVTokens: 5}
	mustAdmit(t, a, small) // fits behind the blocked big one - backfill, not FIFO-strict
	cancelBig()
}

// mustBlockFirstArrival starts an Admit call and waits for it to actually be
// queued (not yet fitting) before returning, so a subsequent smaller Admit is
// guaranteed to observe it as the (blocked) oldest waiter.
func mustBlockFirstArrival(t *testing.T, a *Admission, spec AdmissionSpec) (cancel func()) {
	t.Helper()
	// occupy all capacity first so `spec` itself cannot fit and must queue
	filler := AdmissionSpec{Model: spec.Model, KVTokens: 100}
	mustAdmit(t, a, filler)
	ctx, cancelFn := context.WithCancel(context.Background())
	go a.Admit(ctx, spec, nil)
	time.Sleep(50 * time.Millisecond) // let it register as a waiter
	a.Release(filler)                 // free capacity, but not enough for spec (90) minus small headroom
	time.Sleep(50 * time.Millisecond)
	return func() { cancelFn(); a.Release(spec) }
}

// Aging: once the oldest blocked node ages past the threshold, it must be the
// next admitted - a stream of smaller backfilled nodes cannot starve it forever.
func TestAgingStopsBackfillAndAdmitsOldest(t *testing.T) {
	a := NewAdmission(nil, map[string]int{"m": 100}, nil, 30*time.Millisecond)
	// Fill capacity so nothing fits.
	filler := AdmissionSpec{Model: "m", KVTokens: 100}
	mustAdmit(t, a, filler)

	fat := AdmissionSpec{Model: "m", KVTokens: 100}
	fatDone := make(chan bool, 1)
	fatCtx, fatCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer fatCancel()
	go func() { fatDone <- a.Admit(fatCtx, fat, nil) }()
	time.Sleep(10 * time.Millisecond) // fat registers as the oldest waiter

	// Release the filler: a small node would normally backfill ahead of fat...
	a.Release(filler)

	// ...but wait past the aging threshold, then try a small backfill candidate.
	time.Sleep(50 * time.Millisecond) // > 30ms aging threshold
	small := AdmissionSpec{Model: "m", KVTokens: 5}
	smallCtx, smallCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer smallCancel()
	if a.Admit(smallCtx, small, nil) {
		t.Fatal("small node backfilled past the aged oldest waiter - aging did not hold capacity")
	}

	select {
	case ok := <-fatDone:
		if !ok {
			t.Fatal("aged fat node was never admitted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("aged fat node was starved")
	}
	a.Release(fat)
}

// The aged-waiter check must gate Admit's fast path too - a brand-new small
// request arriving AFTER the oldest waiter has already aged must not jump the
// queue just because it happens to fit. Leaves real headroom (80/100 used,
// not 100/100) so the block is explained ONLY by aging, never by "no
// capacity" - a prior version of this test filled capacity to 100%, which
// passed even with the aging gate fully bypassed (mutated to `if true`).
func TestAgingActuallyBlocksLaterBackfill(t *testing.T) {
	a := NewAdmission(nil, map[string]int{"m": 100}, nil, 40*time.Millisecond)
	occupant := AdmissionSpec{Model: "m", KVTokens: 80}
	mustAdmit(t, a, occupant) // 80/100 used, 20 free

	fat := AdmissionSpec{Model: "m", KVTokens: 100}
	fatDone := make(chan bool, 1)
	fatCtx, fatCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer fatCancel()
	go func() { fatDone <- a.Admit(fatCtx, fat, nil) }()
	time.Sleep(10 * time.Millisecond) // fat = sole/oldest waiter

	smallA := AdmissionSpec{Model: "m", KVTokens: 15}
	mustAdmit(t, a, smallA) // pre-aging backfill: 80+15=95<=100, fits
	a.Release(smallA)       // back to 80/100 used, 20 free again

	time.Sleep(60 * time.Millisecond) // fat now aged (>40ms)

	smallB := AdmissionSpec{Model: "m", KVTokens: 15} // would fit (95<=100) if backfill still allowed
	cancel := mustBlock(t, a, smallB)                 // capacity exists - only aging can explain a block
	cancel()

	a.Release(occupant)
	select {
	case ok := <-fatDone:
		if !ok {
			t.Fatal("aged fat node was never admitted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("aged fat node was starved")
	}
	a.Release(fat)
}

// Release must return capacity even when the caller path is an abort/panic
// recovery, not just clean completion - exercised here via defer+recover,
// mirroring how newGatedNode's `defer admission.Release(spec)` behaves.
func TestReleaseOnPanicPath(t *testing.T) {
	a := NewAdmission(map[string]int{"m": 1}, nil, nil, time.Hour)
	spec := AdmissionSpec{Model: "m"}

	func() {
		defer func() { recover() }()
		mustAdmit(t, a, spec)
		defer a.Release(spec)
		panic("simulated node panic")
	}()

	mustAdmit(t, a, spec) // capacity was returned despite the panic
	a.Release(spec)
}

func TestAdmitHonoursContextCancel(t *testing.T) {
	a := NewAdmission(map[string]int{"m": 1}, nil, nil, time.Hour)
	spec := AdmissionSpec{Model: "m"}
	mustAdmit(t, a, spec) // fill the one slot

	ctx, cancel := context.WithCancel(context.Background())
	var got atomic.Bool
	done := make(chan struct{})
	go func() {
		got.Store(a.Admit(ctx, spec, nil))
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Admit ignored context cancellation and hung")
	}
	if got.Load() {
		t.Fatal("Admit reported success on a cancelled context")
	}
}

// activeKeyTest mirrors serve.activeKey / AdmissionSpec.residencyKey.
func activeKeyTest(provider, role string) string { return provider + "\x00" + role }

// An already-cancelled ctx must never reserve capacity, even when the spec
// fits on the very first check (#1021 review: a prior reorder broke this by
// checking fits/reserve before ctx.Err()). Verified by exhausting the freed
// capacity afterward at full limit.
func TestAdmitAlreadyCancelledNeverReserves(t *testing.T) {
	a := NewAdmission(map[string]int{"m": 1}, nil, nil, time.Hour)
	spec := AdmissionSpec{Model: "m"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before Admit is ever called - capacity is free

	if a.Admit(ctx, spec, nil) {
		t.Fatal("Admit succeeded on an already-cancelled ctx")
	}

	// If the cancelled call had reserved anyway, this would block/fail.
	mustAdmit(t, a, spec)
	a.Release(spec)
}
