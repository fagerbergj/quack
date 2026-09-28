package dag

import (
	"context"
	"iter"
	"sync"
	"sync/atomic"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/vetting"
)

// fixedLLM answers every worker call with reply (after wait, if set) and passes every judge round.
type fixedLLM struct {
	reply string
	wait  <-chan struct{}
}

func (f fixedLLM) Name() string { return f.reply }

func (f fixedLLM) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if gHasTool(req, "submit_verdict") {
			yield(gCall("submit_verdict", map[string]any{"score": 0.9, "feedback": ""}), nil)
			return
		}
		if f.wait != nil {
			<-f.wait
		}
		yield(gText(f.reply), nil)
	}
}

func rosterWith(t *testing.T, llm fixedLLM) *Roster {
	t.Helper()
	a, err := llmagent.New(llmagent.Config{Name: "w", Model: llm, Description: "w", Instruction: "ROLE:w"})
	if err != nil {
		t.Fatalf("agent: %v", err)
	}
	return &Roster{
		Agents: map[string]adkagent.Agent{"w": a}, Models: map[string]model.LLM{"w": llm},
		Infos:  []AgentInfo{{Name: "w", Description: "w"}},
		CfgFor: func(context.Context, string) vetting.Config { return vetting.Config{Threshold: 0.6, JudgeRounds: 1} },
	}
}

func TestRoster_PinnedRunSurvivesSetRoster(t *testing.T) {
	swapped := make(chan struct{})
	old := rosterWith(t, fixedLLM{reply: "FROM-OLD", wait: swapped})
	old.Gen = 1
	var deaths atomic.Int32
	old.OnDead = func() { deaths.Add(1) }
	ex := NewExecutor(session.InMemoryService(), nil, nil, vetting.NewJudgeFactory(fixedLLM{reply: "judge"}, nil, nil), nil, nil)
	ex.SetRoster(old)
	ctx, done := ex.Pin(context.Background())
	fresh := rosterWith(t, fixedLLM{reply: "FROM-NEW"})

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 2; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			ex.SetRoster(&Roster{Gen: uint64(i), Agents: fresh.Agents, Models: fresh.Models, CfgFor: fresh.CfgFor})
			if i == 50 {
				close(swapped)
			}
		}
	}()
	plan := Plan{ID: "p", UserMessage: "go", Nodes: []Node{{ID: "n1", AgentName: "w", Task: "T"}}}
	out, _, _, err := ex.RunPlanStep(ctx, plan, "quack", "u", "chat", nil, map[string]bool{"n1": true})
	close(stop)
	wg.Wait()
	if err != nil {
		t.Fatalf("RunPlanStep: %v", err)
	}
	if out["n1"] != "FROM-OLD" {
		t.Fatalf("n1 = %q, want the pinned roster's FROM-OLD", out["n1"])
	}
	if deaths.Load() != 0 {
		t.Fatal("OnDead fired while a run was still pinned")
	}
	done()
	done()
	if deaths.Load() != 1 {
		t.Fatalf("OnDead fired %d times after the last done, want 1", deaths.Load())
	}
}

func TestRoster_OnDeadOncePerRetiredNeverCurrent(t *testing.T) {
	ex := NewExecutor(nil, nil, nil, nil, nil, nil)
	var mu sync.Mutex
	deaths := map[*Roster]int{}
	mk := func(gen int) *Roster {
		r := &Roster{Gen: uint64(gen)}
		r.OnDead = func() { mu.Lock(); deaths[r]++; mu.Unlock() }
		return r
	}
	var all []*Roster
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				_, done := ex.Pin(context.Background())
				done()
			}
		}()
	}
	for i := range 100 {
		r := mk(i + 1)
		all = append(all, r)
		ex.SetRoster(r)
	}
	wg.Wait()
	cur := ex.RosterFor(context.Background())
	for _, r := range all {
		want := 1
		if r == cur {
			want = 0
		}
		if deaths[r] != want {
			t.Fatalf("roster OnDead count = %d, want %d (current=%v)", deaths[r], want, r == cur)
		}
	}
}

func TestRoster_NestedPinKeepsRoster(t *testing.T) {
	ex := NewExecutor(nil, nil, nil, nil, nil, nil)
	first := &Roster{Gen: 1}
	ex.SetRoster(first)
	ctx, done := ex.Pin(context.Background())
	ex.SetRoster(&Roster{Gen: 2})
	inner, innerDone := ex.Pin(ctx)
	if got := ex.RosterFor(inner).Gen; got != 1 {
		t.Fatalf("nested pin Gen = %d, want 1", got)
	}
	innerDone()
	if first.runs.Load() < 0 {
		t.Fatal("roster died while the outer pin was live")
	}
	done()
	if first.runs.Load() >= 0 {
		t.Fatal("retired roster not dead after its last done")
	}
}

// A ctx that outlived its run (context.WithoutCancel after done) must not revive a dead roster.
func TestRoster_EscapedCtxRepinsCurrent(t *testing.T) {
	ex := NewExecutor(nil, nil, nil, nil, nil, nil)
	var deaths atomic.Int32
	ex.SetRoster(&Roster{Gen: 1, OnDead: func() { deaths.Add(1) }})
	ctx, done := ex.Pin(context.Background())
	escaped := context.WithoutCancel(ctx)
	done()
	ex.SetRoster(&Roster{Gen: 2})
	if got := ex.RosterFor(escaped).Gen; got != 2 {
		t.Fatalf("RosterFor on a dead pin = Gen %d, want current 2", got)
	}
	repinned, redone := ex.Pin(escaped)
	defer redone()
	if got := ex.RosterFor(repinned).Gen; got != 2 {
		t.Fatalf("nested pin on a dead roster = Gen %d, want current 2", got)
	}
	if deaths.Load() != 1 {
		t.Fatalf("OnDead = %d, want 1", deaths.Load())
	}
}

func TestRoster_SetRosterRejectsReuse(t *testing.T) {
	ex := NewExecutor(nil, nil, nil, nil, nil, nil)
	a := &Roster{Gen: 1}
	ex.SetRoster(a)
	ex.SetRoster(&Roster{Gen: 2})
	for name, r := range map[string]*Roster{"nil": nil, "retired": a, "stale gen": {Gen: 2}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("SetRoster(%s) did not panic", name)
				}
			}()
			ex.SetRoster(r)
		}()
	}
	if got := ex.RosterFor(context.Background()).Gen; got != 2 {
		t.Fatalf("current Gen = %d after rejected sets, want 2", got)
	}
}

func TestPlanner_UsesPinnedInfos(t *testing.T) {
	ex := NewExecutor(nil, nil, nil, nil, nil, nil)
	ex.SetRoster(&Roster{Gen: 1, Infos: []AgentInfo{{Name: "fresh"}}})
	p := &Planner{agents: []AgentInfo{{Name: "boot"}}}
	nodes := func(agent string) []RawNode { return []RawNode{{ID: "n", Agent: agent, Task: "t"}} }
	pinned, done := ex.Pin(context.Background())
	defer done()
	if _, err := p.BuildBound(pinned, nodes("fresh"), nil, nil, "m", nil, nil); err != nil {
		t.Fatalf("pinned ctx rejected the pinned roster's agent: %v", err)
	}
	if _, err := p.BuildBound(pinned, nodes("boot"), nil, nil, "m", nil, nil); err == nil {
		t.Fatal("pinned ctx accepted an agent missing from the pinned roster")
	}
	if _, err := p.BuildBound(context.Background(), nodes("boot"), nil, nil, "m", nil, nil); err != nil {
		t.Fatalf("unpinned ctx lost the planner's own agents: %v", err)
	}
	empty := NewExecutor(nil, nil, nil, nil, nil, nil)
	empty.SetRoster(&Roster{Gen: 1})
	emptyCtx, emptyDone := empty.Pin(context.Background())
	defer emptyDone()
	if _, err := p.BuildBound(emptyCtx, nodes("boot"), nil, nil, "m", nil, nil); err == nil {
		t.Fatal("a reload roster with no agents fell back to the boot agents")
	}
}

func TestRoster_SetRosterOwnsItsMaps(t *testing.T) {
	ex := NewExecutor(nil, nil, nil, nil, nil, nil)
	media := map[string]bool{"w": true}
	ex.SetRoster(&Roster{Gen: 1, Media: media})
	media["w"] = false
	if !ex.RosterFor(context.Background()).Media["w"] {
		t.Fatal("caller's map mutation leaked into the installed roster")
	}
}
