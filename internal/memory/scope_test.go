package memory

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledgertest"
)

// The bucket model (shared, subject-keyed memory): a memory belongs to a bucket
// describing WHAT IT IS ABOUT - the repo, the role family, or the user - never to
// the agent that happened to write it. These tests are the contract.

const (
	repoA = "github.com/acme/games"
	repoB = "github.com/acme/other"
)

// addOp is a consolidator that ADDs whatever single fact it is given, verbatim.
func addOp(content string) model.LLM {
	return fakeModel{reply: `{"ops":[{"action":"ADD","content":"` + content + `","kind":"convention"}]}`}
}

// codingView is the memory service a coding agent gets: its own role/legacy base
// scope, plus the per-invocation repo + user resolved from the worker's context.
func codingView(s *Store, agentName, repo, user string) *View {
	return s.View(Scope{Role: RoleCoding, Legacy: agentName}, func(context.Context) Scope {
		return Scope{Repo: repo, User: user}
	})
}

func recall(t *testing.T, v *View, query string) []adkmemory.Entry {
	t.Helper()
	resp, err := v.SearchMemory(context.Background(), &adkmemory.SearchRequest{Query: query})
	if err != nil {
		t.Fatalf("SearchMemory: %v", err)
	}
	if resp == nil {
		return nil
	}
	return resp.Memories
}

// TestRepoBucketSharedByEveryCodingAgent is the whole point of the redesign: what
// the EXPLORER learns about a repo must reach the IMPLEMENTER and the REVIEWER -
// they work on the same subject, so they share the same bucket.
func TestRepoBucketSharedByEveryCodingAgent(t *testing.T) {
	ctx := context.Background()
	const fact = "load-games.ts registers every game; a new game must be added there"
	s := newSQLiteStore(t, "task", addOp(fact))

	// The explorer stages a REPO fact and its answer passes the gate → committed.
	explorer := Scope{Repo: repoA, Role: RoleCoding, User: "u1", Legacy: "code-explorer"}
	if _, err := s.Commit(ctx, explorer, "code-explorer", Provenance{},
		[]Candidate{{Content: fact, Metadata: map[string]string{"bucket": "repo"}}}, ""); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Every coding agent working in THAT repo recalls it.
	for _, name := range []string{"code-implementer", "code-reviewer", "code-explorer"} {
		if got := recall(t, codingView(s, name, repoA, "u1"), "where are games registered"); len(got) != 1 {
			t.Fatalf("%s recalled %d memories, want 1 (the explorer's repo knowledge must reach it)", name, len(got))
		}
	}

	// A DIFFERENT repo does not: the bucket is the subject, and the subject is a repo.
	if got := recall(t, codingView(s, "code-implementer", repoB, "u1"), "where are games registered"); len(got) != 0 {
		t.Fatalf("other repo recalled %d memories, want 0 (repo buckets must not bleed)", len(got))
	}
}

// TestRoleBucketNotSharedAcrossFamilies: tradecraft is shared within a role family
// (coding / research) but not across it - a researcher's fetch tactic is noise to a coder.
func TestRoleBucketNotSharedAcrossFamilies(t *testing.T) {
	ctx := context.Background()
	const fact = "a source's own docs beat a blog post about it"
	s := newSQLiteStore(t, "task", addOp(fact))

	researcher := Scope{Role: RoleResearch, User: "u1", Legacy: "web-researcher"}
	if _, err := s.Commit(ctx, researcher, "web-researcher", Provenance{},
		[]Candidate{{Content: fact, Metadata: map[string]string{"bucket": "role"}}}, ""); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Another researcher (same family) recalls it.
	other := s.View(Scope{Role: RoleResearch, Legacy: "synthesizer"}, func(context.Context) Scope {
		return Scope{User: "u1"}
	})
	if got := recall(t, other, "which sources to trust"); len(got) != 1 {
		t.Fatalf("same-family recall got %d, want 1", len(got))
	}

	// A coding agent does not.
	if got := recall(t, codingView(s, "code-implementer", repoA, "u1"), "which sources to trust"); len(got) != 0 {
		t.Fatalf("coding agent recalled %d research-role memories, want 0", len(got))
	}
}

// TestUserBucketRecalledByEveryone: a fact about the user is everyone's business
// (and nobody else's).
func TestUserBucketRecalledByEveryone(t *testing.T) {
	ctx := context.Background()
	const fact = "the user prefers TypeScript over JavaScript"
	s := newSQLiteStore(t, "task", addOp(fact))

	if _, err := s.Commit(ctx, Scope{User: "u1", Legacy: "u1"}, "orchestrator", Provenance{},
		[]Candidate{{Content: fact, Metadata: map[string]string{"bucket": "user"}}}, ""); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	for _, name := range []string{"code-implementer", "code-explorer"} {
		if got := recall(t, codingView(s, name, repoA, "u1"), "language preference"); len(got) != 1 {
			t.Fatalf("%s recalled %d user memories, want 1", name, len(got))
		}
	}
	researcher := s.View(Scope{Role: RoleResearch, Legacy: "web-researcher"}, func(context.Context) Scope {
		return Scope{User: "u1"}
	})
	if got := recall(t, researcher, "language preference"); len(got) != 1 {
		t.Fatalf("web-researcher recalled %d user memories, want 1", len(got))
	}
	// Another user sees nothing.
	if got := recall(t, codingView(s, "code-implementer", repoA, "u2"), "language preference"); len(got) != 0 {
		t.Fatalf("other user recalled %d memories, want 0 (personal memory must stay isolated)", len(got))
	}
}

// TestLegacyAgentScopedMemoriesStillLoad: memories written before the redesign are
// keyed by the raw agent name (task) or raw user id (user). Reads stay tolerant of
// them - no migration, nothing lost.
func TestLegacyAgentScopedMemoriesStillLoad(t *testing.T) {
	s := newSQLiteStore(t, "task", nil)
	upsertScoped(t, s, "1", "web-researcher", "transportforireland.ie is authoritative for Irish transit")

	v := s.View(Scope{Role: RoleResearch, Legacy: "web-researcher"}, nil)
	if got := recall(t, v, "irish transit"); len(got) != 1 {
		t.Fatalf("legacy agent-scoped memory recalled %d, want 1", len(got))
	}
	// It is still that agent's own memory - nobody else inherits the legacy silo.
	if got := recall(t, codingView(s, "code-implementer", repoA, "u1"), "irish transit"); len(got) != 0 {
		t.Fatalf("legacy silo leaked %d memories to another agent, want 0", len(got))
	}
}

// TestCommitRoutesCandidatesToTheirBuckets: one commit, three subjects - each
// candidate lands in the bucket it declared, and the answer-extraction lands in the
// default bucket (the repo, when the node has one).
func TestCommitRoutesCandidatesToTheirBuckets(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", echoModel{})

	sc := Scope{Repo: repoA, Role: RoleCoding, User: "u1", Legacy: "code-explorer"}
	if _, err := s.Commit(ctx, sc, "code-explorer", Provenance{}, []Candidate{
		{Content: "repo-fact", Metadata: map[string]string{"bucket": "repo"}},
		{Content: "role-fact", Metadata: map[string]string{"bucket": "role"}},
		{Content: "user-fact", Metadata: map[string]string{"bucket": "user"}},
	}, ""); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	for bucket, want := range map[string]string{
		"repo:" + repoA: "repo-fact",
		"role:coding":   "role-fact",
		"user:u1":       "user-fact",
	} {
		pts, err := s.idx.query(ctx, []string{bucket}, []float32{1, 0, 0, 0}, 5)
		if err != nil {
			t.Fatalf("query %s: %v", bucket, err)
		}
		if len(pts) != 1 || pts[0].Content != want {
			t.Fatalf("bucket %s holds %v, want exactly [%s]", bucket, pts, want)
		}
	}
}

// TestCommitFallsBackToRoleWithoutARepo: a deployment (or a node) with no repo
// context must still work - a repo-bucket write with no known repo falls back to the
// role bucket rather than guessing a key.
func TestCommitFallsBackToRoleWithoutARepo(t *testing.T) {
	ctx := context.Background()
	s := newSQLiteStore(t, "task", echoModel{})

	sc := Scope{Role: RoleCoding, User: "u1", Legacy: "code-implementer"} // no Repo
	if _, err := s.Commit(ctx, sc, "code-implementer", Provenance{},
		[]Candidate{{Content: "install deps before running checks", Metadata: map[string]string{"bucket": "repo"}}}, ""); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	pts, err := s.idx.query(ctx, []string{"role:coding"}, []float32{1, 0, 0, 0}, 5)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(pts) != 1 {
		t.Fatalf("role bucket holds %d memories, want 1 (repo-less write must fall back, not vanish)", len(pts))
	}
}

func TestScopeBuckets(t *testing.T) {
	full := Scope{Repo: repoA, Role: RoleCoding, User: "u1", Legacy: "code-explorer"}
	want := []string{"repo:" + repoA, "role:coding", "user:u1", "code-explorer"}
	got := full.Buckets()
	if len(got) != len(want) {
		t.Fatalf("Buckets() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Buckets() = %v, want %v", got, want)
		}
	}
	if got := (Scope{}).Buckets(); len(got) != 0 {
		t.Fatalf("empty scope Buckets() = %v, want none", got)
	}
	// Write routing: declared bucket wins; an unknown/absent one takes the default
	// (repo when known, else role, else user).
	for _, tc := range []struct {
		name string
		sc   Scope
		req  string
		want string
	}{
		{"declared repo", full, "repo", "repo:" + repoA},
		{"declared role", full, "role", "role:coding"},
		{"declared user", full, "user", "user:u1"},
		{"default with repo", full, "", "repo:" + repoA},
		{"default without repo", Scope{Role: RoleCoding, User: "u1"}, "", "role:coding"},
		{"default with user only", Scope{User: "u1"}, "repo", "user:u1"},
		{"nothing known", Scope{}, "repo", ""},
	} {
		if got := tc.sc.writeBucket(tc.req); got != tc.want {
			t.Errorf("%s: writeBucket(%q) = %q, want %q", tc.name, tc.req, got, tc.want)
		}
	}
}

// echoModel is a consolidator that ADDs every staged candidate verbatim - so a test
// can assert WHICH bucket each one landed in.
type echoModel struct{}

func (echoModel) Name() string { return "echo-consolidator" }

func (echoModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	var ops []string
	for _, line := range strings.Split(promptText(req), "\n") {
		if content, ok := strings.CutPrefix(line, "- "); ok {
			ops = append(ops, `{"action":"ADD","content":"`+content+`","kind":"convention"}`)
		}
	}
	reply := `{"ops":[` + strings.Join(ops, ",") + `]}`
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: reply}}}}, nil)
	}
}

// promptText is the consolidation request's user text (the STAGED CANDIDATES block).
func promptText(req *model.LLMRequest) string {
	var b strings.Builder
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// preloadCtx is an agent context whose SearchMemory routes to a View - the real
// recall path preload_memory takes at runtime.
type preloadCtx struct {
	*adkagent.ContextMock
	view  *View
	query string
}

func (c *preloadCtx) UserContent() *genai.Content {
	return &genai.Content{Role: genai.RoleUser, Parts: []*genai.Part{{Text: c.query}}}
}

func (c *preloadCtx) SearchMemory(_ context.Context, query string) (*adkmemory.SearchResponse, error) {
	// ADK hands the invocation context through; ContextMock's has no deadline support,
	// so route the search on a plain one - the View is what's under test here.
	return c.view.SearchMemory(context.Background(), &adkmemory.SearchRequest{Query: query})
}

// TestLoadMemorySearchRecordsRecall: a View wired with WithRecall must log a
// memory.recall ledger entry and bump recalls on every SearchMemory call.
func TestLoadMemorySearchRecordsRecall(t *testing.T) {
	forEachBackend(t, func(t *testing.T, newStore func(string, model.LLM) *Store) {
		ctx := context.Background()
		const fact = "the release train ships every Tuesday"
		s := newStore("task", addOp(fact))

		explorer := Scope{Repo: repoA, Role: RoleCoding, User: "u1", Legacy: "code-explorer"}
		if _, err := s.Commit(ctx, explorer, "code-explorer", Provenance{},
			[]Candidate{{Content: fact, Metadata: map[string]string{"bucket": "repo"}}}, ""); err != nil {
			t.Fatalf("Commit: %v", err)
		}

		lgr := ledgertest.NewMemStore()
		v := codingView(s, "code-implementer", repoA, "u1").WithRecall(lgr, "chat1", "node1")

		resp, err := v.SearchMemory(ctx, &adkmemory.SearchRequest{Query: "release schedule"})
		if err != nil {
			t.Fatalf("SearchMemory: %v", err)
		}
		if len(resp.Memories) != 1 {
			t.Fatalf("SearchMemory returned %d memories, want 1", len(resp.Memories))
		}
		recalledID := resp.Memories[0].ID

		entries, err := lgr.ReadEntries(ctx, "chat1", 0)
		if err != nil {
			t.Fatalf("ReadEntries: %v", err)
		}
		if len(entries) != 1 || entries[0].Kind != ledger.KindMemoryRecall {
			t.Fatalf("entries = %+v, want exactly one memory.recall", entries)
		}
		if entries[0].NodeID != "node1" {
			t.Fatalf("NodeID = %q, want %q", entries[0].NodeID, "node1")
		}
		var payload ledger.MemoryRecallPayload
		if err := json.Unmarshal(entries[0].Payload, &payload); err != nil {
			t.Fatalf("unmarshal payload: %v", err)
		}
		if payload.Source != "tool" || len(payload.Entries) != 1 || payload.Entries[0].ID != recalledID {
			t.Fatalf("payload = %+v, want source=tool with the delivered id", payload)
		}

		get := func() scored {
			pts, err := s.idx.list(ctx, []string{"repo:" + repoA}, 0, 10, true, "", false)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			return pts[0]
		}
		if g := get(); g.Recalls != 1 {
			t.Fatalf("recalls = %d after one load_memory search, want 1", g.Recalls)
		}

		// A second search in the same round (e.g. preload's own automatic search
		// hitting the same point) is its own delivery, not merged into the first.
		if _, err := v.SearchMemory(ctx, &adkmemory.SearchRequest{Query: "release schedule"}); err != nil {
			t.Fatalf("SearchMemory (2nd): %v", err)
		}
		entries2, err := lgr.ReadEntries(ctx, "chat1", 0)
		if err != nil {
			t.Fatalf("ReadEntries: %v", err)
		}
		if len(entries2) != 2 {
			t.Fatalf("entries after 2nd search = %d, want 2 (each delivery is its own entry)", len(entries2))
		}
		if g := get(); g.Recalls != 2 {
			t.Fatalf("recalls = %d after two searches, want 2 (RecordRecall accumulates)", g.Recalls)
		}
	})
}

// TestPreloadInjectsBucketedMemory covers the PRELOAD path end to end: the
// implementer's ambient recall pulls the explorer's repo memory into its request.
func TestPreloadInjectsBucketedMemory(t *testing.T) {
	ctx := context.Background()
	const fact = "run npm ci before any check; a fresh clone has no node_modules"
	s := newSQLiteStore(t, "task", addOp(fact))

	explorer := Scope{Repo: repoA, Role: RoleCoding, User: "u1", Legacy: "code-explorer"}
	if _, err := s.Commit(ctx, explorer, "code-explorer", Provenance{},
		[]Candidate{{Content: fact, Metadata: map[string]string{"bucket": "repo"}}}, ""); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	pc := &preloadCtx{
		ContextMock: &adkagent.ContextMock{},
		view:        codingView(s, "code-implementer", repoA, "u1"),
		query:       "add a new game to the collection",
	}
	req := &model.LLMRequest{Contents: []*genai.Content{pc.UserContent()}}
	if err := NewPreload().(oncePreload).ProcessRequest(pc, req); err != nil {
		t.Fatalf("preload ProcessRequest: %v", err)
	}
	si := req.Config.SystemInstruction
	if si == nil || len(si.Parts) == 0 || !strings.Contains(si.Parts[0].Text, fact) {
		t.Fatalf("preload did not inject the repo memory into the system instruction: %+v", si)
	}
}
