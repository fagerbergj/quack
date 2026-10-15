package dag

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/fagerbergj/quack/internal/stream"
)

// DefaultAgingThreshold: how long the oldest blocked node waits before backfill stops admitting past it.
const DefaultAgingThreshold = 2 * time.Minute

// AdmissionSpec: one node's resolved capacity requirement. Zero values mean "no limit on this
// dimension", never "no capacity".
type AdmissionSpec struct {
	Model    string // models registry key; "" = no session/kv dimension
	KVTokens int    // context tokens this node needs reserved; 0 = not a dimension
	Provider string // provider name for residency accounting; "" with Role "" = no residency dimension
	Role     string
}

func (s AdmissionSpec) residencyKey() string { return s.Provider + "\x00" + s.Role }

// waiter: a blocked Admit call. The spec is kept so aging only holds back waiters that contend with it.
type waiter struct {
	at   time.Time
	spec AdmissionSpec
}

// contends: whether two waiters compete for any same dimension. Aging between non-contending waiters
// would stall a node while the capacity it wants sits idle.
func contends(x, y AdmissionSpec) bool {
	if x.Model != "" && x.Model == y.Model {
		return true
	}
	xr := x.Provider != "" || x.Role != ""
	yr := y.Provider != "" || y.Role != ""
	return xr && yr && x.residencyKey() == y.residencyKey()
}

// Admission is the GPU concurrency limiter: a reclaimable capacity ledger wired in newGatedNode, which both
// DAG paths share. No library fits: x/sync/semaphore refuses to backfill, and per-dimension semaphores deadlock.
type Admission struct {
	mu   sync.Mutex
	cond *sync.Cond

	sessionsLimit map[string]int // model -> cap; absent = unlimited
	sessionsUsed  map[string]int
	kvLimit       map[string]int // model -> cap; absent = unlimited
	kvUsed        map[string]int
	activeLimit   map[string]int            // provider+role key -> cap; absent = unlimited
	residents     map[string]map[string]int // provider+role key -> model -> live node count

	agingThreshold time.Duration
	seq            int64
	waiting        map[int64]waiter // seq -> waiter, present only while blocked in Admit

	// now/afterFunc: clock seam so tests can drive aging deterministically.
	now       func() time.Time
	afterFunc func(time.Duration, func()) timerStopper
}

// timerStopper is the subset of *time.Timer Admit needs, so a fake clock needs no timer goroutine.
type timerStopper interface{ Stop() bool }

// NewAdmission builds the ledger from the models/providers registries; agingThreshold <= 0 uses the default.
func NewAdmission(sessionsLimit, kvLimit, activeLimit map[string]int, agingThreshold time.Duration) *Admission {
	if agingThreshold <= 0 {
		agingThreshold = DefaultAgingThreshold
	}
	a := &Admission{
		sessionsLimit:  sessionsLimit,
		sessionsUsed:   map[string]int{},
		kvLimit:        kvLimit,
		kvUsed:         map[string]int{},
		activeLimit:    activeLimit,
		residents:      map[string]map[string]int{},
		agingThreshold: agingThreshold,
		waiting:        map[int64]waiter{},
		now:            time.Now,
		afterFunc:      func(d time.Duration, fn func()) timerStopper { return time.AfterFunc(d, fn) },
	}
	a.cond = sync.NewCond(&a.mu)
	return a
}

// Admit blocks until spec fits every dimension and reserves it, or returns false on ctx cancel; onQueued fires
// at most once. Oldest-first with backfill, except an oldest waiter aged past agingThreshold goes next.
func (a *Admission) Admit(ctx context.Context, spec AdmissionSpec, onQueued func()) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Register before the fits check: an aged waiter must gate even a new request that would fast-path past it.
	a.seq++
	mySeq := a.seq
	arrived := a.now()
	a.waiting[mySeq] = waiter{at: arrived, spec: spec}
	defer delete(a.waiting, mySeq)
	queuedFired := false

	// Wake on ctx cancellation too, since sync.Cond can't select on a channel.
	stop := context.AfterFunc(ctx, func() {
		a.mu.Lock()
		a.cond.Broadcast()
		a.mu.Unlock()
	})
	defer stop()
	// Nothing guarantees a wakeup when this waiter crosses the aging threshold, so force one.
	agingTimer := a.afterFunc(a.agingThreshold, func() {
		a.mu.Lock()
		a.cond.Broadcast()
		a.mu.Unlock()
	})
	defer agingTimer.Stop()

	for {
		// A dead ctx never reserves and never waits, so it isn't queued either: a paused node's aborted
		// round re-enters here, and a queued event would contradict its pause.
		if ctx.Err() != nil {
			return false
		}
		if (a.oldestContendingSeqLocked(spec) == mySeq || !a.agingActiveLocked(spec)) && a.fits(spec) {
			a.reserve(spec)
			return true
		}
		if !queuedFired && onQueued != nil {
			queuedFired = true
			a.fireUnlocked(onQueued)
			continue // re-check fits: state may have changed while unlocked
		}
		a.cond.Wait()
	}
}

// TryAdmit reserves spec only if it fits now and nobody is queued for the same capacity:
// opportunistic extra work must never jump ahead of a blocked node.
func (a *Admission) TryAdmit(spec AdmissionSpec) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.oldestContendingSeqLocked(spec) != 0 || !a.fits(spec) {
		return false
	}
	a.reserve(spec)
	return true
}

// fireUnlocked calls fn (consumer code, a stream yield) with a.mu released; its defer relocks even if
// fn panics, so Admit's deferred Unlock never double-unlocks.
func (a *Admission) fireUnlocked(fn func()) {
	a.mu.Unlock()
	defer a.mu.Lock()
	fn()
}

// Usage reports spec's current session-dimension load (used, limit) to explain a queued wait;
// ok is false when spec.Model has no configured limit.
func (a *Admission) Usage(spec AdmissionSpec) (used, limit int, ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	limit, ok = a.sessionsLimit[spec.Model]
	return a.sessionsUsed[spec.Model], limit, ok
}

// QueuedEvent is the node_queued event for nodeID blocked on spec, naming the dimension that is full.
func (a *Admission) QueuedEvent(nodeID string, spec AdmissionSpec) stream.SSEEvent {
	return stream.NodeAwaitingCapacity(nodeID, spec.Model, a.waitReason(spec))
}

func (a *Admission) waitReason(spec AdmissionSpec) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if limit, ok := a.sessionsLimit[spec.Model]; ok && a.sessionsUsed[spec.Model]+1 > limit {
		return fmt.Sprintf("%d of %d sessions busy", a.sessionsUsed[spec.Model], limit)
	}
	if limit, ok := a.kvLimit[spec.Model]; ok && spec.KVTokens > 0 && a.kvUsed[spec.Model]+spec.KVTokens > limit {
		return fmt.Sprintf("%d of %d context tokens reserved", a.kvUsed[spec.Model], limit)
	}
	if limit, ok := a.activeLimit[spec.residencyKey()]; ok && !a.fits(spec) {
		return fmt.Sprintf("%d of %d models loaded on %s", len(a.residents[spec.residencyKey()]), limit, spec.Provider)
	}
	return "behind an earlier request"
}

// Release returns spec's reserved capacity and wakes any blocked waiters.
func (a *Admission) Release(spec AdmissionSpec) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if spec.Model != "" {
		if _, ok := a.sessionsLimit[spec.Model]; ok {
			a.sessionsUsed[spec.Model]--
		}
		if spec.KVTokens > 0 {
			if _, ok := a.kvLimit[spec.Model]; ok {
				a.kvUsed[spec.Model] -= spec.KVTokens
			}
		}
	}
	if key := spec.residencyKey(); spec.Provider != "" || spec.Role != "" {
		if m := a.residents[key]; m != nil {
			m[spec.Model]--
			if m[spec.Model] <= 0 {
				delete(m, spec.Model)
			}
		}
	}
	a.cond.Broadcast()
}

// fits/reserve need mu held; fits never mutates, so Admit's check-then-commit stays atomic across dimensions.
func (a *Admission) fits(spec AdmissionSpec) bool {
	if spec.Model != "" {
		if limit, ok := a.sessionsLimit[spec.Model]; ok && a.sessionsUsed[spec.Model]+1 > limit {
			return false
		}
		if spec.KVTokens > 0 {
			if limit, ok := a.kvLimit[spec.Model]; ok && a.kvUsed[spec.Model]+spec.KVTokens > limit {
				return false
			}
		}
	}
	if spec.Provider != "" || spec.Role != "" {
		key := spec.residencyKey()
		if limit, ok := a.activeLimit[key]; ok {
			m := a.residents[key]
			if _, resident := m[spec.Model]; !resident && len(m) >= limit {
				return false // admitting would require evicting a model another live node uses
			}
		}
	}
	return true
}

func (a *Admission) reserve(spec AdmissionSpec) {
	if spec.Model != "" {
		if _, ok := a.sessionsLimit[spec.Model]; ok {
			a.sessionsUsed[spec.Model]++
		}
		if spec.KVTokens > 0 {
			if _, ok := a.kvLimit[spec.Model]; ok {
				a.kvUsed[spec.Model] += spec.KVTokens
			}
		}
	}
	if spec.Provider != "" || spec.Role != "" {
		key := spec.residencyKey()
		m := a.residents[key]
		if m == nil {
			m = map[string]int{}
			a.residents[key] = m
		}
		m[spec.Model]++
	}
}

// oldestContendingSeqLocked: the oldest waiter competing with spec, itself included; seq breaks ties
// so same-instant waiters don't flap on map iteration order.
func (a *Admission) oldestContendingSeqLocked(spec AdmissionSpec) int64 {
	var oldest int64
	var oldestT time.Time
	for seq, w := range a.waiting {
		if !contends(w.spec, spec) {
			continue
		}
		if oldest == 0 || w.at.Before(oldestT) || (w.at.Equal(oldestT) && seq < oldest) {
			oldest, oldestT = seq, w.at
		}
	}
	return oldest
}

// agingActiveLocked: whether the oldest contending waiter has aged past the threshold, so only it may go next.
func (a *Admission) agingActiveLocked(spec AdmissionSpec) bool {
	oldest := a.oldestContendingSeqLocked(spec)
	if oldest == 0 {
		return false
	}
	return a.now().Sub(a.waiting[oldest].at) > a.agingThreshold
}
