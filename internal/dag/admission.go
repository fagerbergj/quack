package dag

import (
	"context"
	"sync"
	"time"
)

// DefaultAgingThreshold: how long the oldest blocked node waits before
// backfill stops admitting past it (see Admission.Admit).
const DefaultAgingThreshold = 2 * time.Minute

// AdmissionSpec: one node's resolved capacity requirement (#1007). Zero
// values mean "no limit on this dimension" - never "no capacity".
type AdmissionSpec struct {
	Model    string // models registry key; "" = no session/kv dimension
	KVTokens int    // context tokens this node needs reserved; 0 = not a dimension
	Provider string // provider name for residency accounting; "" with Role "" = no residency dimension
	Role     string
}

func (s AdmissionSpec) residencyKey() string { return s.Provider + "\x00" + s.Role }

// waiter: a blocked Admit call. The spec is kept so aging only holds back
// waiters that actually contend with it (#1038).
type waiter struct {
	at   time.Time
	spec AdmissionSpec
}

// contends: whether two waiters compete for any same dimension. Aging between
// non-contending waiters is starvation protection nobody asked for - it stalls
// a node while the capacity it wants sits idle (#1038).
func contends(x, y AdmissionSpec) bool {
	if x.Model != "" && x.Model == y.Model {
		return true
	}
	xr := x.Provider != "" || x.Role != ""
	yr := y.Provider != "" || y.Role != ""
	return xr && yr && x.residencyKey() == y.residencyKey()
}

// Admission is a mutex-guarded, reclaimable capacity ledger shared by both
// DAG execution paths (rundag.go and nativegraph.go both run through
// newGatedNode, so wiring Admit/Release there covers both). It replaces
// dag.max_active_runs/max_active_nodes as the GPU concurrency limiter -
// see the #1007 "Settled design" issue comment.
//
// No library composes this: x/sync/semaphore deliberately refuses to
// backfill, and one semaphore per dimension deadlocks across dimensions.
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
}

// NewAdmission builds an Admission ledger from the config's models/providers
// registries. agingThreshold <= 0 uses DefaultAgingThreshold.
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
	}
	a.cond = sync.NewCond(&a.mu)
	return a
}

// Admit blocks until spec fits every dimension (sessions, kv_tokens,
// provider residency), then atomically reserves it, or returns early with
// false if ctx is cancelled. onQueued fires at most once, the first time
// this call would otherwise block - callers use it to emit a `queued` SSE
// event without spamming it on every wakeup.
//
// Queue policy: oldest-waiter-first, backfill (skip a waiter that doesn't
// fit and keep scanning), except once the oldest waiter has been blocked
// past agingThreshold, only it may be admitted next - that stops it being
// starved forever by a stream of smaller backfilled nodes.
func (a *Admission) Admit(ctx context.Context, spec AdmissionSpec, onQueued func()) bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	// Register as a waiter BEFORE the fits check - an aged waiter must gate
	// even a brand-new request that would otherwise fast-path past it.
	a.seq++
	mySeq := a.seq
	arrived := time.Now()
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
	// Nothing else guarantees a wakeup exactly when this waiter crosses the
	// aging threshold (no Release/cancel need ever happen) - force one.
	agingTimer := time.AfterFunc(a.agingThreshold, func() {
		a.mu.Lock()
		a.cond.Broadcast()
		a.mu.Unlock()
	})
	defer agingTimer.Stop()

	for {
		fits := (a.oldestContendingSeqLocked(spec) == mySeq || !a.agingActiveLocked(spec)) && a.fits(spec)
		if fits {
			// Never reserve on a dead ctx, even if capacity happens to be free -
			// cancelled work must not proceed, no matter how it got here (#1016).
			if ctx.Err() != nil {
				return false
			}
			a.reserve(spec)
			return true
		}
		// Contention (not fitting) is "queued" regardless of ctx state, and
		// firing here can never lead to a reservation - unlike the old
		// ctx-after-fits ordering this replaced.
		if !queuedFired && onQueued != nil {
			queuedFired = true
			a.fireUnlocked(onQueued)
			continue // re-check fits: state may have changed while unlocked
		}
		if ctx.Err() != nil {
			return false
		}
		a.cond.Wait()
	}
}

// fireUnlocked calls fn with a.mu released (fn is arbitrary consumer code -
// a stream yield - so it must never run under the lock). Its own defer
// relocks even if fn panics, so Admit's deferred Unlock never double-unlocks.
func (a *Admission) fireUnlocked(fn func()) {
	a.mu.Unlock()
	defer a.mu.Lock()
	fn()
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

// fits/reserve must be called with mu held; fits performs no mutation so
// Admit's all-or-nothing check-then-commit stays atomic across dimensions.
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

// oldestSeqLocked: the lowest (earliest-arrived) currently-waiting seq, or 0 if none.
// oldestContendingSeqLocked: the oldest waiter competing with spec, itself
// included. seq breaks ties - two waiters registered in the same instant would
// otherwise flap on map iteration order.
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

// agingActiveLocked: whether the oldest CONTENDING waiter has aged past the
// threshold - if so, only it may be admitted next (no backfill past it).
func (a *Admission) agingActiveLocked(spec AdmissionSpec) bool {
	oldest := a.oldestContendingSeqLocked(spec)
	if oldest == 0 {
		return false
	}
	return time.Since(a.waiting[oldest].at) > a.agingThreshold
}
