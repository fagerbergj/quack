package serve

import (
	"context"
	"database/sql"
	"iter"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	_ "github.com/glebarez/go-sqlite"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"

	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/orchestrator"
)

// fixedEmbedder returns the same unit vector for every text - enough to
// round-trip a memory through Commit without a real embedding model, same
// stand-in as rest.fixedEmbedder.
type fixedEmbedder struct{}

func (fixedEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0, 0, 0}
	}
	return out, nil
}

// echoConsolidator ADDs each staged candidate verbatim - a stand-in for the
// real consolidation model so a test can seed a recognizable memory (mirrors
// rest.echoConsolidator).
type echoConsolidator struct{}

func (echoConsolidator) Name() string { return "echo-consolidator" }

func (echoConsolidator) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	var text strings.Builder
	for _, c := range req.Contents {
		for _, p := range c.Parts {
			text.WriteString(p.Text)
		}
	}
	staged := text.String()
	if i := strings.Index(staged, "\nEXISTING MEMORIES"); i >= 0 {
		staged = staged[:i]
	}
	var ops []string
	for _, line := range strings.Split(staged, "\n") {
		if content, ok := strings.CutPrefix(line, "- "); ok {
			ops = append(ops, `{"action":"ADD","content":"`+content+`","kind":"repo"}`)
		}
	}
	reply := `{"ops":[` + strings.Join(ops, ",") + `]}`
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{Content: &genai.Content{Parts: []*genai.Part{{Text: reply}}}}, nil)
	}
}

// newMemStoreForTest opens a real SQLite-backed memory.Store at a fresh temp
// file - domain selects the consolidation prompt ("task" | "user"), mirroring
// openMemory's own domain argument in serve.go. Returns the backing file path
// too, so a test can reach in at the raw-SQL level (dropMemoriesTable) to
// force a genuine backend error.
func newMemStoreForTest(t *testing.T, domain string) (*memory.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mem.db")
	s, err := memory.OpenSQLite(context.Background(), path, fixedEmbedder{}, echoConsolidator{}, "test_"+domain, domain, 5, 0)
	if err != nil {
		t.Fatalf("OpenSQLite(%s): %v", domain, err)
	}
	return s, path
}

// dropMemoriesTable opens a second raw connection to s's backing file and
// drops its table, so the next call through s hits a real backend error
// ("no such table") instead of a fake/mocked one - proves
// applyMemoryOutcome's error handling against an actual failure mode.
func dropMemoriesTable(t *testing.T, path string) {
	t.Helper()
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer raw.Close()
	if _, err := raw.Exec("DROP TABLE memories"); err != nil {
		t.Fatalf("drop memories table: %v", err)
	}
}

// fakeOpsLog records every memory_ops write for assertion, mirroring
// internal/memory's own lifecycle_test.go fixture (unexported there, so
// re-declared locally).
type fakeOpsLog struct {
	mu   sync.Mutex
	rows []fakeOpRow
}

type fakeOpRow struct {
	memoryID string
	op       memory.OpsLogOp
	actor    memory.OpsLogActor
	reason   string
}

func (f *fakeOpsLog) LogMemoryOp(_ context.Context, memoryID string, op memory.OpsLogOp, actor memory.OpsLogActor, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, fakeOpRow{memoryID, op, actor, reason})
	return nil
}

func (f *fakeOpsLog) snapshot() []fakeOpRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeOpRow(nil), f.rows...)
}

// seedMemory mints one memory under chatID via the real Commit path (not a
// direct index poke), so it carries the same provenance/status a live run
// would leave behind.
func seedMemory(t *testing.T, s *memory.Store, sc memory.Scope, bucket, chatID, content string) {
	t.Helper()
	n, err := s.Commit(context.Background(), sc, "test", memory.Provenance{ChatID: chatID},
		[]memory.Candidate{{Content: content, Metadata: map[string]string{"bucket": bucket}}}, "")
	if err != nil {
		t.Fatalf("seedMemory Commit: %v", err)
	}
	if n != 1 {
		t.Fatalf("seedMemory Commit wrote %d, want 1", n)
	}
}

func listAll(t *testing.T, s *memory.Store, buckets []string) []memory.Memory {
	t.Helper()
	mems, _, err := s.List(context.Background(), buckets, 0, 10, true)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return mems
}

// TestUpdateChatOriginOpenToClosedInvalidatesBothStores covers design doc §7
// case 1 at the wiring level: a State transition to closed invalidates the
// chat's minted memories in BOTH configured stores, one memory_ops row each
// (actor=outcome-feedback), and the origin update itself succeeds.
func TestUpdateChatOriginOpenToClosedInvalidatesBothStores(t *testing.T) {
	st, orch, hub, artifacts, jail := newExtTestStack(t)
	_ = jail
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var extHolder atomic.Pointer[extsdk.Extension]
	dispatch := newExtDispatch("noop", &orchRef, st, hub, &extHolder, nil, artifacts)

	taskMem, _ := newMemStoreForTest(t, "task")
	userMem, _ := newMemStoreForTest(t, "user")
	updateOrigin := newExtUpdateChatOrigin("noop", st, taskMem, userMem)

	const localID = "closes-unmerged"
	chatID := "ext:noop:" + localID
	origin := &extsdk.ChatOrigin{Extension: "noop", Label: "acme/widgets#9", Kind: "pull_request", Badge: "open", State: extsdk.SubjectOpen}
	req := extsdk.DispatchRequest{Chat: extsdk.ChatRef{LocalID: localID, Origin: origin}, Ask: extsdk.Ask{Message: "hi"}}
	if err := dispatch(context.Background(), req); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	waitRunSettled(t, st, chatID)

	seedMemory(t, taskMem, memory.Scope{Repo: "r"}, "repo", chatID, "a coding convention this run minted")
	seedMemory(t, userMem, memory.Scope{User: "u"}, "user", chatID, "a user fact this run minted")

	// Wired after seeding, so the fake only captures the outcome-feedback
	// rows this test asserts on, not the seed Commit's own ADD rows.
	ops := &fakeOpsLog{}
	taskMem.SetOpsLog(ops)
	userMem.SetOpsLog(ops)

	if err := updateOrigin(localID, extsdk.ChatOrigin{Extension: "noop", Label: "acme/widgets#9", Kind: "pull_request", Badge: "closed", State: extsdk.SubjectClosed}); err != nil {
		t.Fatalf("updateOrigin: %v", err)
	}

	taskMems := listAll(t, taskMem, []string{"repo:r"})
	if len(taskMems) != 1 || taskMems[0].Status != string(memory.StatusInvalidated) || taskMems[0].InvalidationReason != "subject closed unmerged" {
		t.Fatalf("task memory = %+v, want status=invalidated reason=%q", taskMems, "subject closed unmerged")
	}
	userMems := listAll(t, userMem, []string{"user:u"})
	if len(userMems) != 1 || userMems[0].Status != string(memory.StatusInvalidated) || userMems[0].InvalidationReason != "subject closed unmerged" {
		t.Fatalf("user memory = %+v, want status=invalidated reason=%q", userMems, "subject closed unmerged")
	}

	rows := ops.snapshot()
	if len(rows) != 2 {
		t.Fatalf("ops rows = %+v, want exactly 2 (one per store)", rows)
	}
	for _, r := range rows {
		if r.op != memory.OpInvalidate || r.actor != memory.ActorOutcomeFeedback {
			t.Fatalf("op row = %+v, want {op:invalidate actor:outcome-feedback}", r)
		}
	}
}

// TestUpdateChatOriginOpenToMergedReinforces covers design doc §7 case 2 at
// the wiring level: a State transition to merged reinforces (unverified →
// reinforced, count 0→1), and a second update at the same State is a no-op
// (steady state, not a transition).
func TestUpdateChatOriginOpenToMergedReinforces(t *testing.T) {
	st, orch, hub, artifacts, jail := newExtTestStack(t)
	_ = jail
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var extHolder atomic.Pointer[extsdk.Extension]
	dispatch := newExtDispatch("noop", &orchRef, st, hub, &extHolder, nil, artifacts)

	taskMem, _ := newMemStoreForTest(t, "task")
	updateOrigin := newExtUpdateChatOrigin("noop", st, taskMem, nil)

	const localID = "merges-clean"
	chatID := "ext:noop:" + localID
	origin := &extsdk.ChatOrigin{Extension: "noop", Label: "acme/widgets#10", Kind: "pull_request", Badge: "open", State: extsdk.SubjectOpen}
	req := extsdk.DispatchRequest{Chat: extsdk.ChatRef{LocalID: localID, Origin: origin}, Ask: extsdk.Ask{Message: "hi"}}
	if err := dispatch(context.Background(), req); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	waitRunSettled(t, st, chatID)

	seedMemory(t, taskMem, memory.Scope{Repo: "r"}, "repo", chatID, "a repo convention that survived to merge")

	ops := &fakeOpsLog{}
	taskMem.SetOpsLog(ops)

	if err := updateOrigin(localID, extsdk.ChatOrigin{Extension: "noop", Label: "acme/widgets#10", Kind: "pull_request", Badge: "merged", State: extsdk.SubjectMerged}); err != nil {
		t.Fatalf("updateOrigin: %v", err)
	}

	mems := listAll(t, taskMem, []string{"repo:r"})
	if len(mems) != 1 || mems[0].Status != string(memory.StatusReinforced) || mems[0].ReinforcementCount != 1 {
		t.Fatalf("memory = %+v, want status=reinforced count=1", mems)
	}
	if rows := ops.snapshot(); len(rows) != 1 || rows[0].op != memory.OpReinforce || rows[0].actor != memory.ActorOutcomeFeedback {
		t.Fatalf("ops rows = %+v, want exactly 1 {op:reinforce actor:outcome-feedback}", rows)
	}

	// A repeated merged webhook (steady state, not a transition) applies nothing.
	if err := updateOrigin(localID, extsdk.ChatOrigin{Extension: "noop", Label: "acme/widgets#10", Kind: "pull_request", Badge: "merged", State: extsdk.SubjectMerged}); err != nil {
		t.Fatalf("updateOrigin (repeat): %v", err)
	}
	mems = listAll(t, taskMem, []string{"repo:r"})
	if mems[0].ReinforcementCount != 1 {
		t.Fatalf("ReinforcementCount after repeat merged = %d, want still 1", mems[0].ReinforcementCount)
	}
	if rows := ops.snapshot(); len(rows) != 1 {
		t.Fatalf("ops rows after repeat merged = %+v, want still exactly 1", rows)
	}
}

// TestUpdateChatOriginRepeatedClosedAppliesNothing covers design doc §7's
// "repeat webhook" case directly at the State-comparison level: a chat
// dispatched already closed, then updated to closed again, is steady state
// (prev == next) - never a transition, so no outcome fires and no memory_ops
// row is written.
func TestUpdateChatOriginRepeatedClosedAppliesNothing(t *testing.T) {
	st, orch, hub, artifacts, jail := newExtTestStack(t)
	_ = jail
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var extHolder atomic.Pointer[extsdk.Extension]
	dispatch := newExtDispatch("noop", &orchRef, st, hub, &extHolder, nil, artifacts)

	taskMem, _ := newMemStoreForTest(t, "task")
	updateOrigin := newExtUpdateChatOrigin("noop", st, taskMem, nil)

	const localID = "already-closed"
	chatID := "ext:noop:" + localID
	origin := &extsdk.ChatOrigin{Extension: "noop", Label: "acme/widgets#11", Kind: "issues", Badge: "closed", State: extsdk.SubjectClosed}
	req := extsdk.DispatchRequest{Chat: extsdk.ChatRef{LocalID: localID, Origin: origin}, Ask: extsdk.Ask{Message: "hi"}}
	if err := dispatch(context.Background(), req); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	waitRunSettled(t, st, chatID)

	seedMemory(t, taskMem, memory.Scope{Repo: "r"}, "repo", chatID, "a fact minted after this chat had already closed")

	ops := &fakeOpsLog{}
	taskMem.SetOpsLog(ops)

	if err := updateOrigin(localID, extsdk.ChatOrigin{Extension: "noop", Label: "acme/widgets#11", Kind: "issues", Badge: "closed", State: extsdk.SubjectClosed}); err != nil {
		t.Fatalf("updateOrigin: %v", err)
	}

	mems := listAll(t, taskMem, []string{"repo:r"})
	if len(mems) != 1 || mems[0].Status != string(memory.StatusUnverified) {
		t.Fatalf("memory = %+v, want unchanged status=unverified", mems)
	}
	if rows := ops.snapshot(); len(rows) != 0 {
		t.Fatalf("ops rows = %+v, want none", rows)
	}
}

// TestUpdateChatOriginStatelessOriginAppliesNothing covers design doc §7's
// older-extension case: an origin update from an extension pinned below
// sdk v0.5.0 carries State="" - unknown, never treated as a transition into
// (or out of) anything, and never an error.
func TestUpdateChatOriginStatelessOriginAppliesNothing(t *testing.T) {
	st, orch, hub, artifacts, jail := newExtTestStack(t)
	_ = jail
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var extHolder atomic.Pointer[extsdk.Extension]
	dispatch := newExtDispatch("noop", &orchRef, st, hub, &extHolder, nil, artifacts)

	taskMem, _ := newMemStoreForTest(t, "task")
	updateOrigin := newExtUpdateChatOrigin("noop", st, taskMem, nil)

	const localID = "stateless-origin"
	chatID := "ext:noop:" + localID
	origin := &extsdk.ChatOrigin{Extension: "noop", Label: "acme/widgets#12", Kind: "issues", Badge: "open"}
	req := extsdk.DispatchRequest{Chat: extsdk.ChatRef{LocalID: localID, Origin: origin}, Ask: extsdk.Ask{Message: "hi"}}
	if err := dispatch(context.Background(), req); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	waitRunSettled(t, st, chatID)

	seedMemory(t, taskMem, memory.Scope{Repo: "r"}, "repo", chatID, "a fact from a State-less extension version")

	ops := &fakeOpsLog{}
	taskMem.SetOpsLog(ops)

	if err := updateOrigin(localID, extsdk.ChatOrigin{Extension: "noop", Label: "acme/widgets#12", Kind: "issues", Badge: "closed"}); err != nil {
		t.Fatalf("updateOrigin: %v", err)
	}

	mems := listAll(t, taskMem, []string{"repo:r"})
	if len(mems) != 1 || mems[0].Status != string(memory.StatusUnverified) {
		t.Fatalf("memory = %+v, want unchanged status=unverified", mems)
	}
	if rows := ops.snapshot(); len(rows) != 0 {
		t.Fatalf("ops rows = %+v, want none", rows)
	}
}

// TestUpdateChatOriginFollowsStateNotBadge is design doc §5's typed-field
// rule pinned as a test: core must never branch on Badge (display-only). A
// mismatched pair - Badge still reading "open" while State says closed -
// must invalidate, following State alone.
func TestUpdateChatOriginFollowsStateNotBadge(t *testing.T) {
	st, orch, hub, artifacts, jail := newExtTestStack(t)
	_ = jail
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var extHolder atomic.Pointer[extsdk.Extension]
	dispatch := newExtDispatch("noop", &orchRef, st, hub, &extHolder, nil, artifacts)

	taskMem, _ := newMemStoreForTest(t, "task")
	ops := &fakeOpsLog{}
	taskMem.SetOpsLog(ops)
	updateOrigin := newExtUpdateChatOrigin("noop", st, taskMem, nil)

	const localID = "badge-lies"
	chatID := "ext:noop:" + localID
	origin := &extsdk.ChatOrigin{Extension: "noop", Label: "acme/widgets#13", Kind: "pull_request", Badge: "open", State: extsdk.SubjectOpen}
	req := extsdk.DispatchRequest{Chat: extsdk.ChatRef{LocalID: localID, Origin: origin}, Ask: extsdk.Ask{Message: "hi"}}
	if err := dispatch(context.Background(), req); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	waitRunSettled(t, st, chatID)

	seedMemory(t, taskMem, memory.Scope{Repo: "r"}, "repo", chatID, "a fact that must go by State, not Badge")

	// Badge still says "open" - only State moved to closed.
	if err := updateOrigin(localID, extsdk.ChatOrigin{Extension: "noop", Label: "acme/widgets#13", Kind: "pull_request", Badge: "open", State: extsdk.SubjectClosed}); err != nil {
		t.Fatalf("updateOrigin: %v", err)
	}

	mems := listAll(t, taskMem, []string{"repo:r"})
	if len(mems) != 1 || mems[0].Status != string(memory.StatusInvalidated) {
		t.Fatalf("memory = %+v, want status=invalidated (State overrides a stale Badge)", mems)
	}
}

// TestUpdateChatOriginSucceedsDespiteMemoryStoreFailure covers the design
// doc's off-the-hot-path rule: applyMemoryOutcome logs and moves on when a
// configured store's ApplyOutcome fails - the origin update it rides on must
// still succeed. taskMem hits a genuine backend error (its table is dropped
// out from under it, not a mock returning a canned error); userMem is
// entirely absent (nil) - both are the "fake/absent" cases the design calls
// out, exercised together.
func TestUpdateChatOriginSucceedsDespiteMemoryStoreFailure(t *testing.T) {
	st, orch, hub, artifacts, jail := newExtTestStack(t)
	_ = jail
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var extHolder atomic.Pointer[extsdk.Extension]
	dispatch := newExtDispatch("noop", &orchRef, st, hub, &extHolder, nil, artifacts)

	taskMem, taskPath := newMemStoreForTest(t, "task")
	updateOrigin := newExtUpdateChatOrigin("noop", st, taskMem, nil) // userMem absent (nil)

	const localID = "store-breaks"
	chatID := "ext:noop:" + localID
	origin := &extsdk.ChatOrigin{Extension: "noop", Label: "acme/widgets#14", Kind: "pull_request", Badge: "open", State: extsdk.SubjectOpen}
	req := extsdk.DispatchRequest{Chat: extsdk.ChatRef{LocalID: localID, Origin: origin}, Ask: extsdk.Ask{Message: "hi"}}
	if err := dispatch(context.Background(), req); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	waitRunSettled(t, st, chatID)

	dropMemoriesTable(t, taskPath)

	if err := updateOrigin(localID, extsdk.ChatOrigin{Extension: "noop", Label: "acme/widgets#14", Kind: "pull_request", Badge: "closed", State: extsdk.SubjectClosed}); err != nil {
		t.Fatalf("updateOrigin = %v, want nil (a memory store error must not fail the origin update)", err)
	}

	// The origin itself still advanced despite the memory-side failure.
	c, err := st.GetChat(context.Background(), chatID)
	if err != nil || c == nil {
		t.Fatalf("GetChat after update: %v", err)
	}
	if got := priorOriginState(c.Origin); got != extsdk.SubjectClosed {
		t.Fatalf("stored origin state = %q, want %q", got, extsdk.SubjectClosed)
	}
}
