package orchestrator

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/decide/decidetest"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/memory"
)

// chatWithTurns seeds alice's chat1 with events authored as given ("user" or orchestratorName).
func chatWithTurns(t *testing.T, turns ...[2]string) session.Service {
	t.Helper()
	ctx := context.Background()
	svc := session.InMemoryService()
	resp, err := svc.Create(ctx, &session.CreateRequest{AppName: AppName, UserID: "alice", SessionID: "chat1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tn := range turns {
		ev := session.NewEvent(ctx, "")
		ev.Author = tn[0]
		ev.Content = &genai.Content{Role: "model", Parts: []*genai.Part{{Text: tn[1]}}}
		if err := svc.AppendEvent(ctx, resp.Session, ev); err != nil {
			t.Fatal(err)
		}
	}
	return svc
}

func extractTurn(o *Orchestrator, message string) {
	ctx := ledger.WithCoords(context.Background(), ledger.Coords{ChatID: "chat1"})
	o.maybeMineUserMemory(ctx, "alice", "chat1", "", message)()
}

// TestMemoryExtractRecordsTheExtractionBaseline: the record's baseline is whether extraction produced
// a memory, "false" for a pre-filtered turn, "" when extraction failed; prefiltered rides meta, never state.
func TestMemoryExtractRecordsTheExtractionBaseline(t *testing.T) {
	for _, c := range []struct {
		name, message, reply, baseline string
		prefiltered                    bool
	}{
		{"extracted a memory", "From now on always keep it terse.", `[{"content":"User wants terse answers.","kind":"preference"}]`, "true", false},
		{"extracted nothing", "I prefer this one.", `[]`, "false", false},
		{"extraction failed", "I prefer this one.", "[not json]", "", false},
		{"pre-filtered out", "My name is Ada and I run the NightsOut project.", `[]`, "false", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			mem := decidetest.Capture(t)
			states := make(chan json.RawMessage, 1)
			srv := decidetest.Server(t, "durable_fact", 0.97, 0, states)
			called := make(chan struct{}, 1)
			o := &Orchestrator{userMem: newTestStore(t), memAgent: buildScriptedAgentFunc(t, func() { called <- struct{}{} }, c.reply),
				sessions:  chatWithTurns(t, [2]string{"user", c.message}, [2]string{orchestratorName, "Noted, Ada."}),
				decisions: decidetest.Decider(t, srv.URL, memoryExtract.ID)}
			extractTurn(o, c.message)

			got := decidetest.Records(t, mem, "chat1", 1)
			if len(got) != 1 || got[0].Point != "memory.extract" || got[0].Baseline != c.baseline || got[0].Outcome != "observe" {
				t.Fatalf("records = %+v, want one observe record with baseline %q", got, c.baseline)
			}
			if want := `{"prefiltered":` + map[bool]string{true: "true", false: "false"}[c.prefiltered] + `}`; string(got[0].Meta) != want {
				t.Errorf("meta = %s, want %s", got[0].Meta, want)
			}
			state := string(<-states)
			if strings.Contains(state, "Noted, Ada.") || !strings.Contains(state, c.message) || strings.Contains(state, "prefiltered") {
				t.Errorf("state sent = %s, want the message, no assistant reply and no prefiltered flag", state)
			}
			if wasCalled := len(called) > 0; wasCalled == c.prefiltered {
				t.Errorf("memory agent called = %v on a prefiltered=%v turn", wasCalled, c.prefiltered)
			}
		})
	}
}

// TestMemoryExtractNeverDelaysTheTurn: down or hung, the decision service never holds up the hook,
// and extraction still commits.
func TestMemoryExtractNeverDelaysTheTurn(t *testing.T) {
	for name, url := range map[string]string{
		"down": decidetest.Down,
		"hung": decidetest.Server(t, "durable_fact", 0.9, time.Hour, nil).URL,
	} {
		t.Run(name, func(t *testing.T) {
			store := newTestStore(t)
			o := &Orchestrator{userMem: store, memAgent: buildScriptedAgent(t, `[{"content":"User prefers terse, concise answers.","kind":"preference"}]`),
				sessions: chatWithTurns(t), decisions: decidetest.Decider(t, url, memoryExtract.ID)}
			start := time.Now()
			extractTurn(o, "From now on always keep it terse.")
			if d := time.Since(start); d > 200*time.Millisecond {
				t.Errorf("hook took %v; it may only spawn goroutines", d)
			}
			if !waitFor(t, 2*time.Second, func() bool {
				return strings.Contains(store.Recall(context.Background(), memory.Scope{User: "alice", Legacy: "alice"}, "verbosity"), "terse")
			}) {
				t.Error("extraction stopped committing with the decision service " + name)
			}
		})
	}
}

// A disabled point, or no extraction hook to compare against, calls nothing.
func TestMemoryExtractMakesNoCallWithoutABaseline(t *testing.T) {
	srv := decidetest.Server(t, "durable_fact", 0.9, 0, nil)
	enabled := decidetest.Decider(t, srv.URL, memoryExtract.ID)
	for name, o := range map[string]*Orchestrator{
		"disabled": {userMem: newTestStore(t), memAgent: buildScriptedAgent(t, `[]`), sessions: chatWithTurns(t)},
		"no hook":  {userMem: newTestStore(t), sessions: chatWithTurns(t), decisions: enabled},
		"no store": {memAgent: buildScriptedAgent(t, `[]`), sessions: chatWithTurns(t), decisions: enabled},
	} {
		extractTurn(o, "I prefer tabs.")
		time.Sleep(50 * time.Millisecond)
		if n := srv.Calls.Load(); n != 0 {
			t.Fatalf("%s: decision calls = %d, want 0", name, n)
		}
	}
}

// TestMemoryExtractStateIsBounded: a huge turn still fits the handler's cap.
func TestMemoryExtractStateIsBounded(t *testing.T) {
	huge := strings.Repeat("I prefer verbose answers. ", 5000)
	mem := decidetest.Capture(t)
	states := make(chan json.RawMessage, 1)
	srv := decidetest.Server(t, "durable_fact", 0.5, 0, states)
	o := &Orchestrator{userMem: newTestStore(t), memAgent: buildScriptedAgent(t, `[]`),
		sessions: chatWithTurns(t, [2]string{"user", huge}, [2]string{orchestratorName, huge}), decisions: decidetest.Decider(t, srv.URL, memoryExtract.ID)}
	extractTurn(o, huge)
	state := <-states
	if len(state) > 2*memoryExtractMessageMax+100 || strings.Count(string(state), "[truncated]") != 2 {
		t.Errorf("state is %d bytes, want both messages clipped with a marker", len(state))
	}
	decidetest.Records(t, mem, "chat1", 1)
}

// TestMemoryExtractRunsAtTheEndOfARunTurn: Run observes once per turn, after the answer is persisted.
func TestMemoryExtractRunsAtTheEndOfARunTurn(t *testing.T) {
	mem := decidetest.Capture(t)
	states := make(chan json.RawMessage, 1)
	srv := decidetest.Server(t, "durable_fact", 0.9, 0, states)
	o := newTestOrch(t, &orchStub{replies: []*model.LLMResponse{stubText("Tabs it is.")}})
	o.userMem, o.memAgent, o.decisions = newTestStore(t), buildScriptedAgent(t, `[]`), decidetest.Decider(t, srv.URL, memoryExtract.ID)
	for _, err := range o.Run(context.Background(), "u", "chat-run", SourceApp, "I prefer tabs.", nil) {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := decidetest.Records(t, mem, "chat-run", 1); len(got) != 1 || got[0].Baseline != "false" {
		t.Fatalf("records = %+v, want one with baseline false", got)
	}
	if state := string(<-states); strings.Contains(state, "Tabs it is.") || !strings.Contains(state, "I prefer tabs.") || strings.Contains(state, "previous_message") {
		t.Errorf("state = %s, want the turn's message alone: no previous turn, no answer", state)
	}
}

// TestMemoryExtractCarriesThePreviousUserTurn: a second turn's state pairs its message with the
// first turn's, never with itself or either answer.
func TestMemoryExtractCarriesThePreviousUserTurn(t *testing.T) {
	mem := decidetest.Capture(t)
	states := make(chan json.RawMessage, 2)
	srv := decidetest.Server(t, "durable_fact", 0.9, 0, states)
	o := newTestOrch(t, &orchStub{replies: []*model.LLMResponse{stubText("Tabs it is."), stubText("Width 4, noted.")}})
	o.userMem, o.memAgent, o.decisions = newTestStore(t), buildScriptedAgent(t, `[]`), decidetest.Decider(t, srv.URL, memoryExtract.ID)
	var got []memoryExtractState
	for _, msg := range []string{"I prefer tabs.", "Make them width 4."} {
		for _, err := range o.Run(context.Background(), "u", "chat-run", SourceApp, msg, nil) {
			if err != nil {
				t.Fatal(err)
			}
		}
		var st memoryExtractState
		if err := json.Unmarshal(<-states, &st); err != nil {
			t.Fatal(err)
		}
		got = append(got, st)
	}
	decidetest.Records(t, mem, "chat-run", 2)
	want := []memoryExtractState{{Message: "I prefer tabs."}, {Message: "Make them width 4.", Previous: "I prefer tabs."}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("states = %+v, want %+v", got, want)
	}
}
