package dag

import (
	"context"
	"sync"
	"sync/atomic"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/vetting"
)

// Roster is one immutable generation of the agent set: a run pinned to it
// keeps executing against it even after SetRoster installs a newer one.
type Roster struct {
	Gen     uint64
	Agents  map[string]adkagent.Agent
	Models  map[string]model.LLM
	Media   map[string]bool
	Infos   []AgentInfo
	Text    string // the orchestrator prompt's agent list
	CfgFor  func(ctx context.Context, agentName string) vetting.Config
	SpecFor func(agentName string) AdmissionSpec
	// OnDead runs exactly once, after this roster is retired and its last pinned run ends.
	// Set it before SetRoster publishes the roster.
	OnDead func()

	runs    atomic.Int64
	retired atomic.Bool
	dead    atomic.Bool
}

type rosterKey struct{}

// pinnedRoster returns the roster Executor.Pin stored on ctx, or nil.
func pinnedRoster(ctx context.Context) *Roster {
	r, _ := ctx.Value(rosterKey{}).(*Roster)
	return r
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
	if r.retired.Load() && r.runs.Load() == 0 && r.dead.CompareAndSwap(false, true) && r.OnDead != nil {
		r.OnDead()
	}
}

// SetRoster installs r as the roster unpinned runs use and retires the previous one.
func (e *Executor) SetRoster(r *Roster) {
	if old := e.roster.Swap(r); old != nil && old != r {
		old.retire()
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
// A ctx already pinned stays on its roster, so nested pins agree.
func (e *Executor) Pin(ctx context.Context) (context.Context, func()) {
	r := pinnedRoster(ctx)
	if r != nil {
		r.runs.Add(1)
	} else {
		r = e.pinCurrent()
	}
	var once sync.Once
	return context.WithValue(ctx, rosterKey{}, r), func() { once.Do(r.unpin) }
}

// pinCurrent retries when SetRoster retires the loaded roster mid-pin: a
// retired roster may already be dead, so it must never gain a new run.
func (e *Executor) pinCurrent() *Roster {
	for {
		r := e.roster.Load()
		r.runs.Add(1)
		if !r.retired.Load() {
			return r
		}
		r.unpin()
	}
}
