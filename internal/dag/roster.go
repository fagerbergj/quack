package dag

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/vetting"
)

// Roster is one immutable generation of the agent set: a run pinned to it
// keeps executing against it even after SetRoster installs a newer one.
type Roster struct {
	Gen     uint64 // 0 only for NewExecutor's placeholder; SetRoster requires it to grow
	Agents  map[string]adkagent.Agent
	Models  map[string]model.LLM
	Media   map[string]bool
	Infos   []AgentInfo
	Text    string // the orchestrator prompt's agent list
	CfgFor  func(ctx context.Context, agentName string) vetting.Config
	SpecFor func(agentName string) AdmissionSpec
	// OnDead runs once, on whichever goroutine retires the roster or ends its last pinned run,
	// so it must hand blocking work to its own goroutine. Set it before SetRoster.
	OnDead func()

	runs    atomic.Int64 // pinned runs; -1 once dead, so a pin and the death CAS can't both win
	retired atomic.Bool
}

type rosterKey struct{}

// pinnedRoster returns the live roster Executor.Pin stored on ctx, or nil.
func pinnedRoster(ctx context.Context) *Roster {
	if r, _ := ctx.Value(rosterKey{}).(*Roster); r != nil && r.runs.Load() >= 0 {
		return r
	}
	return nil
}

// tryPin counts one more run unless r is dead or retired with nothing left pinned.
func (r *Roster) tryPin() bool {
	for {
		n := r.runs.Load()
		if n < 0 || (n == 0 && r.retired.Load()) {
			return false
		}
		if r.runs.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

func (r *Roster) retire() {
	r.retired.Store(true)
	r.maybeDie()
}

func (r *Roster) unpin() {
	if r.runs.Add(-1) == 0 {
		r.maybeDie()
	}
}

func (r *Roster) maybeDie() {
	if r.retired.Load() && r.runs.CompareAndSwap(0, -1) && r.OnDead != nil {
		r.OnDead()
	}
}

// SetRoster installs r as the roster unpinned runs use and retires the previous
// one. It panics on nil, a roster already installed once, or a Gen that doesn't grow.
func (e *Executor) SetRoster(r *Roster) {
	if r == nil || r.retired.Load() {
		panic("dag: SetRoster needs a fresh roster, not nil or a retired one")
	}
	for {
		old := e.roster.Load()
		if old != nil && r.Gen <= old.Gen {
			panic(fmt.Sprintf("dag: SetRoster Gen %d does not follow current Gen %d", r.Gen, old.Gen))
		}
		if e.roster.CompareAndSwap(old, r) {
			if old != nil {
				old.retire()
			}
			return
		}
	}
}

// RosterFor returns the roster ctx was pinned to, else the current one.
func (e *Executor) RosterFor(ctx context.Context) *Roster {
	if r := pinnedRoster(ctx); r != nil {
		return r
	}
	return e.roster.Load()
}

// Pin stores a roster on ctx for one run and counts it live until done is called.
// A ctx whose pinned roster is still live stays on it, so nested pins agree.
func (e *Executor) Pin(ctx context.Context) (context.Context, func()) {
	r, _ := ctx.Value(rosterKey{}).(*Roster)
	if r == nil || !r.tryPin() {
		r = e.pinCurrent()
	}
	var once sync.Once
	return context.WithValue(ctx, rosterKey{}, r), func() { once.Do(r.unpin) }
}

// pinCurrent retries when SetRoster retires the loaded roster mid-pin; SetRoster
// swaps before it retires, so the retry sees the newer roster.
func (e *Executor) pinCurrent() *Roster {
	for {
		if r := e.roster.Load(); r.tryPin() {
			return r
		}
	}
}
