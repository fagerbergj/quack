package dag

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/vetting"
)

func rosterWith(t *testing.T, llm fnLLM) *Roster {
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
	old := rosterWith(t, fixedLLM("FROM-OLD", swapped))
	old.Gen = 1
	var deaths atomic.Int32
	old.OnDead = func() { deaths.Add(1) }
	ex := NewExecutor(session.InMemoryService(), nil, nil, vetting.NewJudgeFactory(fixedLLM("judge", nil), nil, nil), nil, nil)
	ex.SetRoster(old)
	ctx, done := ex.Pin(context.Background())
	fresh := rosterWith(t, fixedLLM("FROM-NEW", nil))

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

// A pinned run validates plan agents against its own roster, not a newer one.
func TestAgentNamesForUsesPinnedRoster(t *testing.T) {
	ex := NewExecutor(nil, nil, nil, nil, nil, nil)
	ex.SetRoster(&Roster{Gen: 1, Infos: []AgentInfo{{Name: "old"}}})
	ctx, done := ex.Pin(context.Background())
	defer done()
	ex.SetRoster(&Roster{Gen: 2, Infos: []AgentInfo{{Name: "new"}}})
	SetAgentRoster([]AgentInfo{{Name: "new"}})
	if got := AgentNamesFor(ctx); len(got) != 1 || got[0] != "old" {
		t.Fatalf("pinned names = %v, want [old]", got)
	}
	if got := AgentNamesFor(context.Background()); len(got) != 1 || got[0] != "new" {
		t.Fatalf("unpinned names = %v, want the current [new]", got)
	}
	if err := ValidateAgentNameIn("new", AgentNamesFor(ctx)); err == nil {
		t.Fatal("a pinned run accepted an agent only the newer roster has")
	}
}

// A node pinned to a roster whose agent a reload removed still records its
// final status; once that roster dies, the agent stops validating.
func TestDagNodeStatusPersistsAfterReloadDropsItsAgent(t *testing.T) {
	ex := NewExecutor(nil, nil, nil, nil, nil, nil)
	ex.SetRoster(&Roster{Gen: 1, Infos: []AgentInfo{{Name: "hr-probe"}}})
	SetAgentRoster([]AgentInfo{{Name: "hr-probe"}})
	svc := artifact.InMemoryService()
	c := recordstore.New(svc, "quack", "u1", "chat1")
	rec := DagNodeRecord{NodeID: "hr-probe-1", Agent: "hr-probe", Status: StatusQueued}
	if _, _, err := c.SaveStructured(context.Background(), kindDagNode, rec, rec.NodeID, recordstore.Lineage{NodeID: rec.NodeID}); err != nil {
		t.Fatal(err)
	}
	_, done := ex.Pin(context.Background())
	ex.SetRoster(&Roster{Gen: 2, Infos: []AgentInfo{{Name: "other"}}})
	SetAgentRoster([]AgentInfo{{Name: "other"}})

	for _, st := range []NodeStatus{StatusRunning, StatusDone} {
		if err := UpdateDagNodeStatus(context.Background(), svc, "quack", "u1", "chat1", rec.NodeID, st); err != nil {
			t.Fatalf("status %s: %v", st, err)
		}
	}
	raw, _, _, err := c.Latest(context.Background(), kindDagNode+":"+rec.NodeID)
	if err != nil || !strings.Contains(string(raw), `"status":"done"`) {
		t.Fatalf("record = %s (%v), want done", raw, err)
	}
	done()
	if err := validateDagNode(raw); err == nil {
		t.Fatal("a dead roster's agent still validates")
	}
}
