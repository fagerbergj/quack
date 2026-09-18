// admission_wiring_test.go: proves each wrapped model client reserves and
// releases through the shared Admission ledger, not just the orchestrator's.
package serve

import (
	"context"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/skilltoolset"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/skillsource"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/workspace"
)

// blockingLLM enters GenerateContent, blocks until released, then yields one
// complete text response - lets a test observe capacity held mid-call.
type blockingLLM struct {
	entered chan struct{}
	release chan struct{}
	text    string
}

func (f *blockingLLM) Name() string { return "blocking" }

func (f *blockingLLM) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		f.entered <- struct{}{}
		<-f.release
		yield(&model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: f.text}}}}, nil)
	}
}

// newTextStubProvider serves a fixed OpenAI-compatible chat.completion answering
// with a plain assistant message (no tool call), for text-only callers.
func newTextStubProvider(t *testing.T, text string) config.ProviderConfig {
	t.Helper()
	body := fmt.Sprintf(`{"id":"1","object":"chat.completion","model":"m","choices":[{"index":0,"finish_reason":"stop",`+
		`"message":{"role":"assistant","content":%q}}]}`, text)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return config.ProviderConfig{Kind: "openai", Endpoint: srv.URL, APIKey: "k"}
}

// TestBuildAgents_PlanJudgeReservesAndReleases: with the judge model's sole
// session pre-occupied, the plan judge call must block until it's released.
func TestBuildAgents_PlanJudgeReservesAndReleases(t *testing.T) {
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	builtinSkillSrc := newSkillSource(nil)
	skillSrc := skillsource.New(builtinSkillSrc, jail, localUserID)
	skillTS, err := skilltoolset.New(context.Background(), skilltoolset.Config{Source: skillSrc})
	if err != nil {
		t.Fatal(err)
	}
	newScopedSkillTS := func(names []string) (*skilltoolset.SkillToolset, error) {
		src := skillsource.New(skillsource.Scoped(builtinSkillSrc, names), jail, localUserID)
		return skilltoolset.New(context.Background(), skilltoolset.Config{Source: src})
	}
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{"judge-test": newPlanJudgeStubProvider(t)},
		Models: map[string]config.ModelConfig{
			"judge-model": {Provider: "judge-test"},
		},
		Gates: config.GatesConfig{
			Rubric: "be good",
			Judge: config.JudgeConfig{
				Provider: "judge-test", Model: "judge-model", MaxRounds: 1,
				Threshold: 0.7, MaxIterations: 2,
			},
		},
		Workspace: config.WorkspaceConfig{Sandbox: "none"},
	}

	admission := dag.NewAdmission(map[string]int{"judge-model": 1}, nil, nil, 0)
	occupySpec := dag.AdmissionSpec{Model: "judge-model"}
	if !admission.Admit(context.Background(), occupySpec, nil) {
		t.Fatal("could not pre-occupy the judge model's sole session")
	}

	var setupFn dag.SetupFunc
	_, _, nodeServers, _, planJudge, _, _, err := buildAgents(cfg, nil, session.InMemoryService(), skillTS, builtinSkillSrc, newScopedSkillTS,
		nil, nil, jail, nil, nil, nil, nil, nil, nil, nil, nil, nil, &setupFn, nil, nil, nil, admission)
	if err != nil {
		t.Fatalf("buildAgents: %v", err)
	}
	defer nodeServers.closeAll()

	result := make(chan error, 1)
	go func() {
		_, _, cerr := planJudge(context.Background(), "do a thing", "node a: do the thing", "")
		result <- cerr
	}()

	select {
	case err := <-result:
		t.Fatalf("plan judge call returned (err=%v) while its sole session was held - it never reserved capacity", err)
	case <-time.After(100 * time.Millisecond):
	}

	admission.Release(occupySpec)

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("plan judge call: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("plan judge call never completed after its session was released")
	}

	if !admission.Admit(context.Background(), occupySpec, nil) {
		t.Fatal("plan judge call did not release its session")
	}
	admission.Release(occupySpec)
}

// TestClassifyModel_ReservesAndReleases: also the code path behind title
// generation and hook classification - a contending Admit must fail mid-call.
func TestClassifyModel_ReservesAndReleases(t *testing.T) {
	cfg := &config.Config{
		Models: map[string]config.ModelConfig{
			"judge-model": {Provider: "p", Role: "worker"},
		},
	}
	admission := dag.NewAdmission(map[string]int{"judge-model": 1}, nil, nil, 0)
	fake := &blockingLLM{entered: make(chan struct{}), release: make(chan struct{}), text: "a title"}
	wrapped := dag.NewAdmittingLLM(fake, admission, lightweightSpec(cfg, "judge-model"), nil)

	result := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		out, cerr := classifyWithModel(context.Background(), wrapped, "classify this")
		if cerr != nil {
			errCh <- cerr
			return
		}
		result <- out
	}()
	<-fake.entered

	occupySpec := dag.AdmissionSpec{Model: "judge-model"}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if admission.Admit(ctx, occupySpec, nil) {
		admission.Release(occupySpec)
		t.Fatal("a second caller was admitted while the classify call held the model's sole session")
	}

	close(fake.release)

	select {
	case out := <-result:
		if out != "a title" {
			t.Fatalf("classifyWithModel result = %q, want %q", out, "a title")
		}
	case cerr := <-errCh:
		t.Fatalf("classifyWithModel: %v", cerr)
	case <-time.After(time.Second):
		t.Fatal("classifyWithModel never returned")
	}

	if !admission.Admit(context.Background(), occupySpec, nil) {
		t.Fatal("classify call did not release its session")
	}
	admission.Release(occupySpec)
}

// TestBuildUserMemoryHookAgent_ReservesAndReleases: the hook fires from its own
// background goroutine, so it must reserve capacity like any other caller.
func TestBuildUserMemoryHookAgent_ReservesAndReleases(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{"hook-test": newTextStubProvider(t, "[]")},
		Models: map[string]config.ModelConfig{
			"hook-model": {Provider: "hook-test"},
		},
	}
	res := artifactsrc.New("", nil, 0)
	h := config.UserMemoryHookConfig{Enabled: true, Provider: "hook-test", Model: "hook-model"}

	admission := dag.NewAdmission(map[string]int{"hook-model": 1}, nil, nil, 0)
	occupySpec := dag.AdmissionSpec{Model: "hook-model"}
	if !admission.Admit(context.Background(), occupySpec, nil) {
		t.Fatal("could not pre-occupy the hook model's sole session")
	}

	memAgent, err := buildUserMemoryHookAgent(context.Background(), h, cfg, res, nil, admission)
	if err != nil {
		t.Fatalf("buildUserMemoryHookAgent: %v", err)
	}

	r, err := runner.New(runner.Config{
		AppName: "hook-test-app", Agent: memAgent,
		SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "I always want tabs"}}}

	result := make(chan error, 1)
	go func() {
		for _, rerr := range r.Run(context.Background(), "u1", "s1", content, adkagent.RunConfig{}) {
			if rerr != nil {
				result <- rerr
				return
			}
		}
		result <- nil
	}()

	select {
	case err := <-result:
		t.Fatalf("hook call returned (err=%v) while its sole session was held - it never reserved capacity", err)
	case <-time.After(100 * time.Millisecond):
	}

	admission.Release(occupySpec)

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("hook call: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hook call never completed after its session was released")
	}

	if !admission.Admit(context.Background(), occupySpec, nil) {
		t.Fatal("hook call did not release its session")
	}
	admission.Release(occupySpec)
}

// TestOrchestratorWrap_Unchanged: the same wrap assembleOrchestrator uses, now
// fed a ledger built outside it - the block/release contract must still hold.
func TestOrchestratorWrap_Unchanged(t *testing.T) {
	cfg := &config.Config{
		Orchestrator: config.OrchestratorConfig{Model: "orch-model"},
		Models: map[string]config.ModelConfig{
			"orch-model": {Provider: "p", Role: "worker"},
		},
	}
	admission := dag.NewAdmission(map[string]int{"orch-model": 1}, nil, nil, 0)
	fake := &blockingLLM{entered: make(chan struct{}), release: make(chan struct{})}
	orchLLM := dag.NewAdmittingLLM(fake, admission, orchestratorSpec(cfg), nil)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range orchLLM.GenerateContent(context.Background(), nil, false) { //nolint:revive // consuming for effect
		}
	}()
	<-fake.entered

	occupySpec := dag.AdmissionSpec{Model: "orch-model"}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if admission.Admit(ctx, occupySpec, nil) {
		admission.Release(occupySpec)
		t.Fatal("a second caller was admitted while the orchestrator turn held the only session")
	}

	close(fake.release)
	<-done

	if !admission.Admit(context.Background(), occupySpec, nil) {
		t.Fatal("orchestrator turn did not release its session")
	}
	admission.Release(occupySpec)
}

// TestBuildCompaction_FallbackSummarizerReservesAndReleases: the fallback
// runs from a node's compaction hook, outside its own worker reservation.
func TestBuildCompaction_FallbackSummarizerReservesAndReleases(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{"comp-test": newTextStubProvider(t, "ok")},
		Models: map[string]config.ModelConfig{
			"comp-model": {Provider: "comp-test"},
		},
	}
	cfg.Session.Compaction.Enabled = true
	cfg.Session.Compaction.Provider = "comp-test"
	cfg.Session.Compaction.Model = "comp-model"

	admission := dag.NewAdmission(map[string]int{"comp-model": 1}, nil, nil, 0)
	occupySpec := dag.AdmissionSpec{Model: "comp-model"}
	if !admission.Admit(context.Background(), occupySpec, nil) {
		t.Fatal("could not pre-occupy the compaction model's sole session")
	}

	res := artifactsrc.New("", nil, 0)
	compactionFor, err := buildCompaction(cfg, res, nil, admission)
	if err != nil {
		t.Fatalf("buildCompaction: %v", err)
	}
	comp := compactionFor(config.AgentConfig{ContextWindow: 1000}, nil)
	if comp.Summarizer == nil {
		t.Fatal("Compaction.Summarizer is nil, want the wrapped fallback")
	}

	result := make(chan error, 1)
	go func() {
		for _, gerr := range comp.Summarizer.GenerateContent(context.Background(), &model.LLMRequest{}, false) {
			if gerr != nil {
				result <- gerr
				return
			}
		}
		result <- nil
	}()

	select {
	case err := <-result:
		t.Fatalf("fallback summarizer call returned (err=%v) while its sole session was held", err)
	case <-time.After(100 * time.Millisecond):
	}

	admission.Release(occupySpec)

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("fallback summarizer call: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fallback summarizer call never completed after its session was released")
	}

	if !admission.Admit(context.Background(), occupySpec, nil) {
		t.Fatal("fallback summarizer call did not release its session")
	}
	admission.Release(occupySpec)
}

// TestBuildAdvisorAgent_ReservesAndReleases: ask_advisor runs as its own
// nested runner.Run, outside any DAG node's round, so it must reserve too.
func TestBuildAdvisorAgent_ReservesAndReleases(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{"judge-test": newTextStubProvider(t, "ok")},
		Models: map[string]config.ModelConfig{
			"judge-model": {Provider: "judge-test"},
		},
		Gates: config.GatesConfig{
			Rubric: "be good",
			Judge: config.JudgeConfig{
				Provider: "judge-test", Model: "judge-model", MaxRounds: 1,
				Threshold: 0.7, MaxIterations: 2,
			},
		},
	}
	res := artifactsrc.New("", nil, 0)

	admission := dag.NewAdmission(map[string]int{"judge-model": 1}, nil, nil, 0)
	occupySpec := dag.AdmissionSpec{Model: "judge-model"}
	if !admission.Admit(context.Background(), occupySpec, nil) {
		t.Fatal("could not pre-occupy the advisor model's sole session")
	}

	advisorAgent := buildAdvisorAgent(context.Background(), cfg, res, nil, admission)
	if advisorAgent == nil {
		t.Fatal("buildAdvisorAgent returned nil, want a built advisor")
	}

	r, err := runner.New(runner.Config{
		AppName: "advisor-test-app", Agent: advisorAgent,
		SessionService: session.InMemoryService(), AutoCreateSession: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	content := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "should I do X?"}}}

	result := make(chan error, 1)
	go func() {
		for _, rerr := range r.Run(context.Background(), "u1", "s1", content, adkagent.RunConfig{}) {
			if rerr != nil {
				result <- rerr
				return
			}
		}
		result <- nil
	}()

	select {
	case err := <-result:
		t.Fatalf("advisor call returned (err=%v) while its sole session was held", err)
	case <-time.After(100 * time.Millisecond):
	}

	admission.Release(occupySpec)

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("advisor call: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("advisor call never completed after its session was released")
	}

	if !admission.Admit(context.Background(), occupySpec, nil) {
		t.Fatal("advisor call did not release its session")
	}
	admission.Release(occupySpec)
}

// newEmbeddingStubProvider serves a fixed OpenAI-compatible /embeddings response,
// for a memory store's construction-time dimension probe.
func newEmbeddingStubProvider(t *testing.T) config.ProviderConfig {
	t.Helper()
	body := `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2,0.3]}],"model":"e","usage":{"prompt_tokens":1,"total_tokens":1}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return config.ProviderConfig{Kind: "openai", Endpoint: srv.URL, APIKey: "k"}
}

// TestOpenMemoryStores_WrapsConsolidator: the consolidator fires from a
// background sweep, so it must reserve capacity like any other caller.
func TestOpenMemoryStores_WrapsConsolidator(t *testing.T) {
	cfg := &config.Config{
		Providers: map[string]config.ProviderConfig{
			"embed-test":  newEmbeddingStubProvider(t),
			"consol-test": newTextStubProvider(t, "ok"),
		},
		Models: map[string]config.ModelConfig{
			"consol-model": {Provider: "consol-test"},
		},
		Tools: map[string]config.ToolConfig{"stage_memory": {Store: "vec"}},
		Stores: map[string]config.StoreConfig{
			"vec": {
				Kind: "sqlite", URL: filepath.Join(t.TempDir(), "mem.db"), Collection: "task_memory",
				Embedder:      &config.ProviderModel{Provider: "embed-test", Model: "embed-model"},
				Consolidation: &config.ConsolidationConfig{Provider: "consol-test", Model: "consol-model"},
			},
		},
	}
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	admission := dag.NewAdmission(map[string]int{"consol-model": 1}, nil, nil, 0)
	b := &boot{cfg: cfg, admission: admission}
	taskStore, _, _, _, err := b.initMemory(context.Background(), st, nil)
	if err != nil {
		t.Fatalf("initMemory: %v", err)
	}
	if taskStore == nil {
		t.Fatal("initMemory did not build the task memory store")
	}
}
