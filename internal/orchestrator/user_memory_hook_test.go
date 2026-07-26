package orchestrator

import (
	"context"
	"fmt"
	"iter"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/memory"
)

// --- pre-filter ---

func TestUserMemoryPreFilter(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    bool
	}{
		{"stated preference", "I prefer terse answers, keep it short.", true},
		{"standing rule", "From now on always open a PR instead of pushing a branch.", true},
		{"proceed vs ask", "Don't ask me before you proceed, just go ahead.", true},
		{"trivial request", "Can you review this code?", false},
		{"trivial question", "What files changed in the last commit?", false},
		{"empty message", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := userMemoryPreFilter.MatchString(tt.message); got != tt.want {
				t.Errorf("userMemoryPreFilter.MatchString(%q) = %v, want %v", tt.message, got, tt.want)
			}
		})
	}
}

// --- mineUserMemory (agent invocation + parsing) ---

// scriptedModel is a model.LLM that always replies with the same scripted text,
// for driving a memory-agent stand-in without a real model.
type scriptedModel struct{ reply string }

func (scriptedModel) Name() string { return "scripted-memory-agent" }

func (s scriptedModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: s.reply}}},
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil)
	}
}

func buildScriptedAgent(t *testing.T, reply string) adkagent.Agent {
	t.Helper()
	ag, err := llmagent.New(llmagent.Config{
		Name:        "test-memory-agent",
		Description: "test double",
		Model:       scriptedModel{reply: reply},
		Instruction: "reply with the scripted text",
	})
	if err != nil {
		t.Fatalf("llmagent.New: %v", err)
	}
	return ag
}

func TestMineUserMemory(t *testing.T) {
	tests := []struct {
		name    string
		reply   string
		want    []memory.Candidate
		wantErr bool
	}{
		{
			name:  "clean json array",
			reply: `[{"content":"User prefers concise answers.","kind":"preference"}]`,
			want:  []memory.Candidate{{Content: "User prefers concise answers.", Metadata: map[string]string{"kind": "preference"}}},
		},
		{
			name:  "empty array - nothing to commit",
			reply: `[]`,
			want:  nil,
		},
		{
			name: "wrapped in prose and a code fence",
			reply: "Here you go:\n```json\n" +
				`[{"content":"User wants PRs opened as drafts.","kind":"preference"}]` +
				"\n```\nDone.",
			want: []memory.Candidate{{Content: "User wants PRs opened as drafts.", Metadata: map[string]string{"kind": "preference"}}},
		},
		{
			name:  "multiple candidates",
			reply: `[{"content":"User prefers Go.","kind":"preference"},{"content":"User wants to learn Rust.","kind":"goal"}]`,
			want: []memory.Candidate{
				{Content: "User prefers Go.", Metadata: map[string]string{"kind": "preference"}},
				{Content: "User wants to learn Rust.", Metadata: map[string]string{"kind": "goal"}},
			},
		},
		{
			name:  "blank content entries are dropped",
			reply: `[{"content":"  ","kind":"preference"},{"content":"User prefers terse replies.","kind":"preference"}]`,
			want:  []memory.Candidate{{Content: "User prefers terse replies.", Metadata: map[string]string{"kind": "preference"}}},
		},
		{
			name:  "no array in reply - treated as nothing to commit",
			reply: "I cannot help with that.",
			want:  nil,
		},
		{
			name:    "invalid json inside the array",
			reply:   `[{"content": not valid}]`,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ag := buildScriptedAgent(t, tt.reply)
			got, err := mineUserMemory(context.Background(), ag, "some message")
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got candidates %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("mineUserMemory: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d candidates, want %d: %+v", len(got), len(tt.want), got)
			}
			for i := range got {
				if got[i].Content != tt.want[i].Content || got[i].Metadata["kind"] != tt.want[i].Metadata["kind"] {
					t.Errorf("candidate[%d] = %+v, want %+v", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// --- commitUserMemory (scoped write) ---

// fakeEmbedder returns a fixed unit vector for every text, matching internal/memory's
// own test fixture - any query matches any stored point, so only the scope filter
// decides what recall sees.
type fakeEmbedder struct{}

func (fakeEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0, 0, 0}
	}
	return out, nil
}

// stagedCandidateLine matches one "- [kind] content" or "- content" line inside
// the consolidation prompt's STAGED CANDIDATES section (internal/memory/commit.go
// decide()).
var stagedCandidateLine = regexp.MustCompile(`(?m)^- (?:\[(\w+)\] )?(.+)$`)

// fakeConsolidator is a model.LLM standing in for the memory store's
// consolidation model: it turns every staged candidate straight into an ADD op,
// skipping any real reconciliation - enough to exercise Commit's write path
// without hitting a real model.
type fakeConsolidator struct{}

func (fakeConsolidator) Name() string { return "fake-consolidator" }

func (fakeConsolidator) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		var text string
		for _, c := range req.Contents {
			for _, p := range c.Parts {
				if p != nil {
					text += p.Text
				}
			}
		}
		staged := text
		if i := strings.Index(text, "EXISTING MEMORIES"); i >= 0 {
			staged = text[:i]
		}
		var ops []string
		for _, m := range stagedCandidateLine.FindAllStringSubmatch(staged, -1) {
			kind, content := m[1], m[2]
			ops = append(ops, fmt.Sprintf(`{"action":"ADD","content":%q,"kind":%q}`, content, kind))
		}
		body := fmt.Sprintf(`{"ops":[%s]}`, strings.Join(ops, ","))
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: body}}},
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil)
	}
}

func newTestStore(t *testing.T) *memory.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mem.db")
	s, err := memory.OpenSQLite(context.Background(), path, fakeEmbedder{}, fakeConsolidator{}, "test_user", "user", 5, 0.5)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	return s
}

func TestCommitUserMemoryScopedToUser(t *testing.T) {
	store := newTestStore(t)
	cands := []memory.Candidate{{Content: "User prefers concise responses.", Metadata: map[string]string{"kind": "preference"}}}

	commitUserMemory(context.Background(), store, "alice", cands)

	aliceScope := memory.Scope{User: "alice", Legacy: "alice"}
	if got := store.Recall(context.Background(), aliceScope, "response style"); !strings.Contains(got, "concise") {
		t.Errorf("alice recall = %q, want it to contain the committed fact", got)
	}

	bobScope := memory.Scope{User: "bob", Legacy: "bob"}
	if got := store.Recall(context.Background(), bobScope, "response style"); strings.Contains(got, "concise") {
		t.Errorf("bob recall = %q, want alice's fact to stay out of bob's scope", got)
	}
}

func TestCommitUserMemoryNoCandidatesIsNoop(t *testing.T) {
	store := newTestStore(t)
	commitUserMemory(context.Background(), store, "alice", nil) // must not panic on nil store or empty candidates
	commitUserMemory(context.Background(), nil, "alice", []memory.Candidate{{Content: "x"}})
}

// --- maybeMineUserMemory (gating, async, error-swallowing) ---

// waitFor polls cond until it's true or timeout elapses - standard technique
// for asserting on a fire-and-forget goroutine's eventual effect.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

func TestMaybeMineUserMemory_TrivialMessageNeverWakesAgent(t *testing.T) {
	store := newTestStore(t)
	called := false
	var mu sync.Mutex
	ag := buildScriptedAgentFunc(t, func() { mu.Lock(); called = true; mu.Unlock() }, `[]`)

	o := &Orchestrator{userMem: store, memAgent: ag}
	o.maybeMineUserMemory(context.Background(), "alice", "Can you review this code?")

	// Give any (wrongly) spawned goroutine a chance to run before asserting absence.
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if called {
		t.Error("pre-filter should have skipped the memory agent for a trivial message")
	}
}

func TestMaybeMineUserMemory_ToggleOffNeverRuns(t *testing.T) {
	store := newTestStore(t)
	o := &Orchestrator{userMem: store, memAgent: nil} // hook not wired (config disabled)
	o.maybeMineUserMemory(context.Background(), "alice", "I always prefer terse answers.")

	time.Sleep(50 * time.Millisecond)
	got := store.Recall(context.Background(), memory.Scope{User: "alice", Legacy: "alice"}, "terse")
	if got != "" {
		t.Errorf("hook should never run when memAgent is nil, got recall %q", got)
	}
}

func TestMaybeMineUserMemory_FiresAndCommitsScoped(t *testing.T) {
	store := newTestStore(t)
	ag := buildScriptedAgent(t, `[{"content":"User prefers terse, concise answers.","kind":"preference"}]`)

	o := &Orchestrator{userMem: store, memAgent: ag}
	o.maybeMineUserMemory(context.Background(), "alice", "From now on always keep it terse.")

	ok := waitFor(t, 2*time.Second, func() bool {
		return strings.Contains(store.Recall(context.Background(), memory.Scope{User: "alice", Legacy: "alice"}, "verbosity"), "terse")
	})
	if !ok {
		t.Fatal("expected the mined preference to be committed to alice's user bucket")
	}
}

func TestMaybeMineUserMemory_AgentErrorIsSwallowed(t *testing.T) {
	store := newTestStore(t)
	ag := buildScriptedAgent(t, "not json at all")

	o := &Orchestrator{userMem: store, memAgent: ag}

	done := make(chan struct{})
	go func() {
		o.maybeMineUserMemory(context.Background(), "alice", "I always prefer terse answers.")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("maybeMineUserMemory should return immediately regardless of what the async work does")
	}

	// Give the goroutine time to fail and confirm nothing was committed - the
	// error must be logged, not propagated or retried into a bad write.
	time.Sleep(100 * time.Millisecond)
	if got := store.Recall(context.Background(), memory.Scope{User: "alice", Legacy: "alice"}, "terse"); got != "" {
		t.Errorf("a parse error should commit nothing, got recall %q", got)
	}
}

func TestMaybeMineUserMemory_NeverBlocksTheCaller(t *testing.T) {
	store := newTestStore(t)
	release := make(chan struct{})
	ag := buildBlockingAgent(t, release)
	defer close(release)

	o := &Orchestrator{userMem: store, memAgent: ag}

	done := make(chan struct{})
	go func() {
		o.maybeMineUserMemory(context.Background(), "alice", "I always prefer terse answers.")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("maybeMineUserMemory blocked on the (deliberately slow) memory agent")
	}
}

// buildScriptedAgentFunc is buildScriptedAgent plus a side-effect hook invoked
// whenever the model is called - used to prove a call did or didn't happen.
func buildScriptedAgentFunc(t *testing.T, onCall func(), reply string) adkagent.Agent {
	t.Helper()
	ag, err := llmagent.New(llmagent.Config{
		Name:        "test-memory-agent",
		Description: "test double",
		Model:       hookedModel{onCall: onCall, reply: reply},
		Instruction: "reply with the scripted text",
	})
	if err != nil {
		t.Fatalf("llmagent.New: %v", err)
	}
	return ag
}

type hookedModel struct {
	onCall func()
	reply  string
}

func (hookedModel) Name() string { return "hooked-memory-agent" }

func (m hookedModel) GenerateContent(_ context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if m.onCall != nil {
			m.onCall()
		}
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: m.reply}}},
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil)
	}
}

// buildBlockingAgent never replies until release is closed - simulates a slow
// model call, to prove the hook never blocks its caller on it.
func buildBlockingAgent(t *testing.T, release <-chan struct{}) adkagent.Agent {
	t.Helper()
	ag, err := llmagent.New(llmagent.Config{
		Name:        "test-blocking-memory-agent",
		Description: "test double",
		Model:       blockingModel{release: release},
		Instruction: "reply with the scripted text",
	})
	if err != nil {
		t.Fatalf("llmagent.New: %v", err)
	}
	return ag
}

type blockingModel struct{ release <-chan struct{} }

func (blockingModel) Name() string { return "blocking-memory-agent" }

func (m blockingModel) GenerateContent(ctx context.Context, _ *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		select {
		case <-m.release:
		case <-ctx.Done():
			return
		}
		yield(&model.LLMResponse{
			Content:      &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "[]"}}},
			FinishReason: genai.FinishReasonStop,
			TurnComplete: true,
		}, nil)
	}
}
