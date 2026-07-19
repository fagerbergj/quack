package orchestrator

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// A server-wide run cap must make the (N+1)th concurrent run WAIT for a slot, not run.
//
// max_active_nodes bounds nodes within one plan; nothing bounded the number of plans,
// so a burst of webhooks (or REST calls) fanned out unbounded onto one model — the exact
// thrash that made five concurrent PR reviews crawl.
func TestMaxActiveRunsQueuesTheOverflow(t *testing.T) {
	o := &Orchestrator{}
	o.SetMaxActiveRuns(2)

	var inFlight, peak int64
	acq := func() func() {
		rel, acquired := o.acquireRun(context.Background())
		if !acquired {
			t.Fatal("acquireRun reported !acquired on an uncancelled context")
		}
		n := atomic.AddInt64(&inFlight, 1)
		for {
			p := atomic.LoadInt64(&peak)
			if n <= p || atomic.CompareAndSwapInt64(&peak, p, n) {
				break
			}
		}
		return func() { atomic.AddInt64(&inFlight, -1); rel() }
	}

	// Two runs hold their slots...
	r1, r2 := acq(), acq()

	// ...a third must BLOCK, not acquire.
	got := make(chan func(), 1)
	go func() { got <- acq() }()
	select {
	case <-got:
		t.Fatal("the 3rd run acquired a slot while 2 were held — the cap is not enforced")
	case <-time.After(100 * time.Millisecond):
		// correctly blocked
	}

	// Free one; the third now proceeds.
	r1()
	select {
	case r3 := <-got:
		r3()
	case <-time.After(2 * time.Second):
		t.Fatal("the 3rd run never acquired after a slot freed — it is stuck")
	}
	r2()

	if peak > 2 {
		t.Fatalf("peak concurrency was %d, cap was 2 — more runs ran at once than allowed", peak)
	}
}

// Unlimited (n < 1) never blocks — a slot is always free.
func TestMaxActiveRunsUnlimited(t *testing.T) {
	o := &Orchestrator{} // no SetMaxActiveRuns → unlimited
	for i := 0; i < 50; i++ {
		rel, acquired := o.acquireRun(context.Background())
		if !acquired {
			t.Fatal("acquireRun reported !acquired when unlimited")
		}
		rel() // acquire+release, must never block
	}
}

// A run waiting for a slot must give up when its context is cancelled, not hang forever.
func TestAcquireRunHonoursContextCancel(t *testing.T) {
	o := &Orchestrator{}
	o.SetMaxActiveRuns(1)
	hold, acquired := o.acquireRun(context.Background()) // fill the one slot
	if !acquired {
		t.Fatal("acquireRun reported !acquired filling the first slot")
	}
	defer hold()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_, acquired := o.acquireRun(ctx)
		if acquired {
			t.Error("acquireRun reported acquired on a cancelled context")
		}
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("acquireRun ignored context cancellation and hung")
	}
}

// Queued must report true for exactly the window between admission and slot
// acquisition — the same window Run() marks via queuedChats — and false both
// before admission and once a slot is held. Backs the REST status handler's
// queued-vs-running split (#417).
func TestQueuedReportsAdmittedButNotAcquired(t *testing.T) {
	const chatID = "chat-1"
	o := &Orchestrator{}
	o.SetMaxActiveRuns(1)

	if o.Queued(chatID) {
		t.Fatal("Queued reported true before admission")
	}

	// Fill the one slot with an unrelated run so chatID's own acquireRun blocks —
	// mirrors Run()'s admission-then-acquire sequence around the semaphore.
	holderRelease, acquired := o.acquireRun(context.Background())
	if !acquired {
		t.Fatal("acquireRun reported !acquired filling the first slot")
	}

	o.queuedChats.Store(chatID, struct{}{})
	if !o.Queued(chatID) {
		t.Fatal("Queued reported false while admitted and waiting on the semaphore")
	}

	got := make(chan func(), 1)
	go func() {
		rel, acquired := o.acquireRun(context.Background())
		if !acquired {
			t.Error("acquireRun reported !acquired on an uncancelled context")
		}
		o.queuedChats.Delete(chatID)
		got <- rel
	}()

	// Still queued while the slot is held elsewhere.
	select {
	case <-got:
		t.Fatal("chatID's run acquired a slot while the only slot was held")
	case <-time.After(100 * time.Millisecond):
	}
	if !o.Queued(chatID) {
		t.Fatal("Queued flipped false before the slot was actually acquired")
	}

	holderRelease()
	select {
	case rel := <-got:
		defer rel()
	case <-time.After(2 * time.Second):
		t.Fatal("chatID's run never acquired after the slot freed")
	}
	if o.Queued(chatID) {
		t.Fatal("Queued still reports true after the slot was acquired")
	}
}

// A run cancelled while queued (never acquiring a slot) must decrement runs.queued,
// not runs.active — the bug this fix addresses (#417): a queued-but-not-executing
// run inflated quack.runs.active.
func TestAcquireRunContractDistinguishesQueuedFromAcquired(t *testing.T) {
	o := &Orchestrator{}
	o.SetMaxActiveRuns(1)
	hold, acquired := o.acquireRun(context.Background())
	if !acquired {
		t.Fatal("acquireRun reported !acquired filling the first slot")
	}
	defer hold()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: the second acquireRun must not block on runSem
	rel, acquired := o.acquireRun(ctx)
	if acquired {
		t.Fatal("acquireRun reported acquired despite the slot being held and ctx cancelled")
	}
	rel() // release must be a safe no-op when !acquired
}
