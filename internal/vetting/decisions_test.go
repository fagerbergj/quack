package vetting

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/decide"
	"github.com/fagerbergj/quack/internal/decide/decidetest"
	"github.com/fagerbergj/quack/internal/ledger"
)

// TestAnswerAcceptObservesEachJudgeRound: one record per judge round, baseline that round's verdict,
// and the gate's own outcome is identical whether the service agrees, is down, or hangs.
func TestAnswerAcceptObservesEachJudgeRound(t *testing.T) {
	for _, c := range []struct {
		name, url, outcome string
	}{
		{"service up", decidetest.Server(t, "pass", 0.99, 0, nil).URL, "observe"},
		{"service down", decidetest.Down, "unavailable"},
		{"service hung", decidetest.Server(t, "pass", 0.99, time.Hour, nil).URL, "unavailable"},
	} {
		t.Run(c.name, func(t *testing.T) {
			mem := decidetest.Capture(t)
			stub := &stubModel{}
			cfg := Config{JudgeRounds: 2, Threshold: 0.7, Rubric: "score the answer 0-10", ChatID: "chat-aa", Task: "What is the capital of France?",
				Decisions: decidetest.Decider(t, c.url, answerAccept.ID)}
			start := time.Now()
			res := runGatedStub(t, stub, nil, NewJudgeFactory(stub, nil, nil), cfg)
			if d := time.Since(start); d > 1500*time.Millisecond {
				t.Errorf("gate took %v; the decision service delayed it", d)
			}
			if !res.Passed || res.Rounds != 2 || stub.judgeCalls != 2 || stub.workerCalls != 2 {
				t.Fatalf("gate = %+v, judge %d worker %d calls; want fail-then-pass in 2 rounds as without the point", res, stub.judgeCalls, stub.workerCalls)
			}
			got := decidetest.Records(t, mem, "chat-aa", 2)
			var baselines []string
			for _, r := range got {
				if r.Point != "answer.accept" || r.Outcome != c.outcome {
					t.Errorf("record = %+v, want an answer.accept %s record", r, c.outcome)
				}
				baselines = append(baselines, r.Baseline)
			}
			if slices.Sort(baselines); !slices.Equal(baselines, []string{"false", "true"}) {
				t.Errorf("baselines = %v, want one failing and one passing round", baselines)
			}
		})
	}
}

func TestAnswerAcceptDisabledMakesNoCall(t *testing.T) {
	srv := decidetest.Server(t, "pass", 0.5, 0, nil)
	stub := &stubModel{}
	cfg := Config{JudgeRounds: 2, Threshold: 0.7, Rubric: "score", ChatID: "chat-off", Decisions: decidetest.Decider(t, srv.URL, researchSource.ID)}
	runGatedStub(t, stub, nil, NewJudgeFactory(stub, nil, nil), cfg)
	time.Sleep(50 * time.Millisecond)
	if n := srv.Calls.Load(); n != 0 {
		t.Errorf("decision calls = %d, want 0 with answer.accept off", n)
	}
}

func TestAnswerAcceptStateIsBounded(t *testing.T) {
	states := make(chan json.RawMessage, 1)
	srv := decidetest.Server(t, "pass", 0.5, 0, states)
	fetched := map[string]struct{}{}
	var answer strings.Builder
	for i := range 300 {
		u := fmt.Sprintf("https://example.test/page/%04d", i)
		fetched[u] = struct{}{}
		fmt.Fprintf(&answer, "Claim %d ([src](%s)). ", i, u)
	}
	answer.WriteString("Sources: the end.")
	j := &judgeRounds{answer: answer.String(), cfg: Config{Request: strings.Repeat("ask ", 2000), Task: strings.Repeat("task ", 2000),
		Decisions: decidetest.Decider(t, srv.URL, answerAccept.ID)}}
	<-j.observeAnswer(context.Background(), workerActivity{fetched: fetched})("true")
	var st answerAcceptState
	raw := <-states
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	if len(raw) > 12500 || !strings.HasSuffix(st.Request, "[truncated]") || !strings.HasSuffix(st.NodeTask, "[truncated]") ||
		len(st.Answer) > answerAcceptAnswerMin || !strings.Contains(st.Answer, "[truncated]") || !strings.HasPrefix(st.Answer, "Claim 0 ") ||
		!strings.HasSuffix(st.Answer, "Sources: the end.") ||
		len(st.Cited) == 0 || len(strings.Join(st.Cited, "")) > answerAcceptSourcesMax || len(strings.Join(st.Fetched, "")) > answerAcceptSourcesMax {
		t.Errorf("state is %d bytes (request %d, task %d, answer %d, cited %d, fetched %d); want every field clipped, the answer at both ends",
			len(raw), len(st.Request), len(st.NodeTask), len(st.Answer), len(st.Cited), len(st.Fetched))
	}
}

// TestAnswerAcceptStateSeparatesRequestFromTask: the user's words and the planner's assignment
// travel as separate fields, and the answer's budget grows with the handler's input cap.
func TestAnswerAcceptStateSeparatesRequestFromTask(t *testing.T) {
	answer := strings.Repeat("a", 15000) + " Sources: [1]"
	for _, c := range []struct {
		maxTokens int
		whole     bool
	}{{8192, true}, {4096, false}, {0, false}} {
		states := make(chan json.RawMessage, 1)
		srv := decidetest.Server(t, "pass", 0.5, 0, states)
		j := &judgeRounds{answer: answer, cfg: Config{Request: "Is the GIL off?", Task: "Check python3.13t; do NOT conflate the binary with the GIL being off.",
			Decisions: decidetest.DeciderWithCap(t, srv.URL, answerAccept.ID, c.maxTokens)}}
		<-j.observeAnswer(context.Background(), workerActivity{})("true")
		raw := <-states
		var st answerAcceptState
		if err := json.Unmarshal(raw, &st); err != nil {
			t.Fatal(err)
		}
		if st.Request != "Is the GIL off?" || !strings.HasPrefix(st.NodeTask, "Check python3.13t") || strings.Contains(string(raw), `"task"`) {
			t.Errorf("cap %d: state = %.200s, want request and node_task as separate fields", c.maxTokens, raw)
		}
		if (st.Answer == answer) != c.whole || !strings.HasSuffix(st.Answer, "Sources: [1]") || len(raw) > max(c.maxTokens*3, 9000) {
			t.Errorf("cap %d: answer %d bytes (whole %v), state %d bytes; want whole=%v with its tail kept", c.maxTokens, len(st.Answer), st.Answer == answer, len(raw), c.whole)
		}
	}
}

type countingLoader struct {
	fakeLoader
	calls atomic.Int32
}

func (c *countingLoader) Latest(ctx context.Context, id string) ([]byte, int, bool, error) {
	c.calls.Add(1)
	return c.fakeLoader.Latest(ctx, id)
}

// TestResearchSourceAsksPerPageInTurn: pages are asked one at a time, capped at 20 with the rest
// counted as skipped, a page without stored text is not asked, and the baseline is whether the answer cites the page.
func TestResearchSourceAsksPerPageInTurn(t *testing.T) {
	mem := decidetest.Capture(t)
	srv := decidetest.Server(t, "relevant", 0.8, 5*time.Millisecond, nil)
	var urls []string
	pages := fakeLoader{}
	for i := range 26 {
		u := fmt.Sprintf("https://example.test/p%02d", i)
		urls = append(urls, u)
		if i != 3 {
			pages[pageID(t, u)] = "# Page " + u + "\n\nbody"
		}
	}
	answer := "See [one](" + urls[1] + "/#frag) and " + pageID(t, urls[2]) + "."
	ctx := ledger.WithCoords(context.Background(), ledger.Coords{ChatID: "chat-rs"})
	observePages(ctx, decidetest.Decider(t, srv.URL, researchSource.ID), pages, "Which page is it?", "Which page?", answer, urls)

	got := decidetest.Records(t, mem, "chat-rs", 19)
	if len(got) != 19 || srv.MaxInFlight.Load() != 1 {
		t.Fatalf("records = %d, max in flight = %d; want 19 (20 capped, 1 unstored) asked one at a time", len(got), srv.MaxInFlight.Load())
	}
	for _, r := range got {
		var st researchSourceState
		var meta struct{ Skipped, Pages int }
		_ = json.Unmarshal(r.State, &st)
		_ = json.Unmarshal(r.Meta, &meta)
		want := st.URL == urls[1] || st.URL == urls[2]
		if r.Baseline != fmt.Sprint(want) || meta.Skipped != 6 || meta.Pages != 20 || st.Title != "Page "+st.URL || st.NodeTask != "Which page?" || st.Request != "Which page is it?" {
			t.Errorf("record for %s: baseline %s meta %s title %q", st.URL, r.Baseline, r.Meta, st.Title)
		}
	}
}

// TestResearchSourceSharesOneLane: concurrent researcher nodes still put one research.source request in flight.
func TestResearchSourceSharesOneLane(t *testing.T) {
	decidetest.Capture(t)
	srv := decidetest.Server(t, "relevant", 0.8, 5*time.Millisecond, nil)
	d := decidetest.Decider(t, srv.URL, researchSource.ID)
	var wg sync.WaitGroup
	for n := range 4 {
		pages, urls := fakeLoader{}, []string{}
		for i := range 3 {
			u := fmt.Sprintf("https://example.test/n%d/p%d", n, i)
			urls, pages[pageID(t, u)] = append(urls, u), "body"
		}
		wg.Go(func() { observePages(context.Background(), d, pages, "r", "q", "", urls) })
	}
	wg.Wait()
	if c, m := srv.Calls.Load(), srv.MaxInFlight.Load(); c != 12 || m != 1 {
		t.Errorf("calls = %d, max in flight = %d; want 12 calls, one at a time", c, m)
	}
}

// TestResearchSourceRunsAtNodeEnd: a web-researcher's fetched page is asked about once its answer is final.
func TestResearchSourceRunsAtNodeEnd(t *testing.T) {
	mem := decidetest.Capture(t)
	srv := decidetest.Server(t, "relevant", 0.8, 0, nil)
	u := "https://example.test/paris"
	fetch, err := functiontool.New[stubFetchArgs, stubFetchResult](functiontool.Config{Name: "web_fetch", Description: "Fetch pages."},
		func(_ adkagent.Context, a stubFetchArgs) (stubFetchResult, error) {
			return stubFetchResult{Results: []map[string]string{{"url": u, "text": "stored"}}}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{JudgeRounds: 1, Threshold: 0.5, Rubric: "score", ChatID: "chat-end", Agent: researchSourceAgentName, Task: "Capital of France?",
		RecordReader: fakeLoader{pageID(t, u): "Paris\n\nParis is the capital."}, Decisions: decidetest.Decider(t, srv.URL, researchSource.ID)}
	runGatedStub(t, fetchingWorker{url: u, answer: "Paris ([src](" + u + "))."}, []tool.Tool{fetch}, NewJudgeFactory(stubPassJudge{}, nil, nil), cfg)
	if got := decidetest.Records(t, mem, "chat-end", 1); len(got) != 1 || got[0].Point != "research.source" || got[0].Baseline != "true" {
		t.Errorf("records = %+v, want one research.source record citing the page", got)
	}
}

// TestResearchSourceGating: another agent's node, or the point disabled, reads no page and calls nothing;
// enabled with a hung service, the node end still returns at once.
func TestResearchSourceGating(t *testing.T) {
	srv := decidetest.Server(t, "relevant", 0.8, time.Hour, nil)
	on := decidetest.Decider(t, srv.URL, researchSource.ID)
	act := workerActivity{fetched: map[string]struct{}{"https://example.test/a": {}}}
	for _, c := range []struct {
		name  string
		agent string
		d     *decide.Decider
		calls int32
	}{
		{"other agent", "code-implementer", on, 0},
		{"disabled", researchSourceAgentName, decidetest.Decider(t, srv.URL, answerAccept.ID), 0},
		{"enabled, service hung", researchSourceAgentName, on, 1},
	} {
		load := &countingLoader{fakeLoader: fakeLoader{pageID(t, "https://example.test/a"): "A\n\ntext"}}
		g := &gateRun{nodeCtx: context.Background(), cfg: Config{Agent: c.agent, Decisions: c.d, RecordReader: load}}
		start := time.Now()
		g.observeSources("answer", act)
		if d := time.Since(start); d > 50*time.Millisecond {
			t.Errorf("%s: observeSources took %v; it may only spawn a goroutine", c.name, d)
		}
		time.Sleep(50 * time.Millisecond)
		if load.calls.Load() != c.calls || srv.Calls.Load() > c.calls {
			t.Errorf("%s: page loads %d, decision calls %d; want %d", c.name, load.calls.Load(), srv.Calls.Load(), c.calls)
		}
	}
}

func TestResearchSourceStateIsBounded(t *testing.T) {
	u := "https://example.test/" + strings.Repeat("x", 1000)
	page := strings.Repeat("T", 500) + "\n" + strings.Repeat("body text ", 5000) + "References: end."
	for _, maxTokens := range []int{0, 8192} {
		states := make(chan json.RawMessage, 1)
		srv := decidetest.Server(t, "relevant", 0.5, 0, states)
		observePages(context.Background(), decidetest.DeciderWithCap(t, srv.URL, researchSource.ID, maxTokens), fakeLoader{pageID(t, u): page},
			strings.Repeat("r ", 2000), strings.Repeat("q ", 2000), "", []string{u})
		raw := <-states
		var st researchSourceState
		if err := json.Unmarshal(raw, &st); err != nil {
			t.Fatal(err)
		}
		budget := max(researchTextMin, maxTokens*3-1024-len(st.Request)-len(st.NodeTask)-len(st.Title)-len(st.URL))
		if len(raw) > max(10500, maxTokens*3) || len(st.Text) > budget || len(st.Text) < budget-10 || !strings.HasSuffix(st.Text, "References: end.") ||
			len(st.Title) > researchTitleMax || len(st.URL) > researchURLMax || len(st.NodeTask) > researchTaskMax || len(st.Request) > requestMax {
			t.Errorf("cap %d: state is %d bytes (request %d, task %d, title %d, url %d, text %d of %d); want every field clipped, the text at both ends",
				maxTokens, len(raw), len(st.Request), len(st.NodeTask), len(st.Title), len(st.URL), len(st.Text), budget)
		}
	}
}

// TestAnswerAcceptBaselineIsTheJudgesVerdict: a WAL save failure that forces the round closed
// after the judge passed it still records the judge's pass.
func TestAnswerAcceptBaselineIsTheJudgesVerdict(t *testing.T) {
	mem := decidetest.Capture(t)
	fl := newFakeGateLedger()
	fl.failKind, fl.failOccurrence = "judge_round", 2
	stub := &stubModel{}
	cfg := Config{JudgeRounds: 2, Threshold: 0.7, Rubric: "score", Artifact: kindText, ChatID: "chat-wal", User: "u1",
		Artifacts: artifact.InMemoryService(), Ledger: fl, Decisions: decidetest.Decider(t, decidetest.Down, answerAccept.ID)}
	if res := runGatedStub(t, stub, nil, NewJudgeFactory(stub, nil, nil), cfg); res.Passed {
		t.Fatal("the failed WAL save should have forced the gate closed")
	}
	var baselines []string
	for _, r := range decidetest.Records(t, mem, "chat-wal", 2) {
		baselines = append(baselines, r.Baseline)
	}
	if slices.Sort(baselines); !slices.Equal(baselines, []string{"false", "true"}) {
		t.Errorf("baselines = %v, want the judge's fail then pass", baselines)
	}
}
