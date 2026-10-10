package serve

import (
	"context"
	"database/sql"
	"encoding/json"
	"iter"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/glebarez/go-sqlite"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledgertest"
	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/runlog"
)

// fixedEmbedder returns one unit vector for every text: enough to round-trip a memory through Commit.
type fixedEmbedder struct{}

func (fixedEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, 0, 0, 0}
	}
	return out, nil
}

// echoConsolidator ADDs each staged candidate verbatim, so a test can seed a recognizable memory.
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

// newMemStoreForTest opens a SQLite memory.Store for domain ("task" | "user"); the returned path lets
// dropMemoriesTable force a real backend error.
func newMemStoreForTest(t *testing.T, domain string) (*memory.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mem.db")
	s, err := memory.OpenSQLite(context.Background(), path, fixedEmbedder{}, echoConsolidator{}, "test_"+domain, domain, 5, 0)
	if err != nil {
		t.Fatalf("OpenSQLite(%s): %v", domain, err)
	}
	return s, path
}

// dropMemoriesTable drops s's table over a second raw connection, so the next call hits a real
// "no such table" error rather than a mock.
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

// fakeOpsLog records memory_ops writes (internal/memory's equivalent fixture is unexported).
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

func (f *fakeOpsLog) PruneMemoryOps(_ context.Context, _ time.Time) (int, error) {
	return 0, nil
}

func (f *fakeOpsLog) snapshot() []fakeOpRow {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeOpRow(nil), f.rows...)
}

// seedMemory mints a memory via the real Commit path and records it RECALLED into chatID with a
// memory.recall ledger entry (applyMemoryOutcome targets the recalled set). Returns its id.
func seedMemory(t *testing.T, s *memory.Store, ledgerStore ledger.LedgerStore, sc memory.Scope, bucket, chatID, content string) string {
	t.Helper()
	n, err := s.Commit(context.Background(), sc, "test", memory.Provenance{ChatID: chatID},
		[]memory.Candidate{{Content: content, Metadata: map[string]string{"bucket": bucket}}}, "")
	if err != nil {
		t.Fatalf("seedMemory Commit: %v", err)
	}
	if n != 1 {
		t.Fatalf("seedMemory Commit wrote %d, want 1", n)
	}
	mems := listAll(t, s, sc.Buckets())
	var id string
	for _, m := range mems {
		if m.Content == content {
			id = m.ID
		}
	}
	if id == "" {
		t.Fatalf("seedMemory: minted memory for %q not found", content)
	}
	payload, err := json.Marshal(ledger.MemoryRecallPayload{Source: "prefill", Entries: []ledger.MemoryRecallEntry{{ID: id}}})
	if err != nil {
		t.Fatalf("seedMemory: marshal recall payload: %v", err)
	}
	if _, err := ledgerStore.AppendIntent(context.Background(), ledger.Entry{
		ChatID: chatID, Kind: ledger.KindMemoryRecall, At: time.Now().UTC(), Payload: payload,
	}); err != nil {
		t.Fatalf("seedMemory: append memory.recall: %v", err)
	}
	return id
}

// newLedgerStoreForTest returns a fresh in-memory ledger for a test's
// memory.recall entries - applyMemoryOutcome's source of the recalled set.
func newLedgerStoreForTest() *ledgertest.MemStore { return ledgertest.NewMemStore() }

func listAll(t *testing.T, s *memory.Store, buckets []string) []memory.Memory {
	t.Helper()
	mems, _, err := s.List(context.Background(), buckets, 0, 10, true, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return mems
}

// TestUpdateChatOriginOpenToClosedInvalidatesBothStores: a transition to closed invalidates the chat's
// memories in both stores, one memory_ops row each (actor=outcome-feedback).
func TestUpdateChatOriginOpenToClosedInvalidatesBothStores(t *testing.T) {
	st, orch, hub, artifacts, jail := newExtTestStack(t)
	_ = jail
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var extHolder atomic.Pointer[extsdk.Extension]
	dispatch := newExtDispatch("noop", &orchRef, st, hub, runlog.NewEventLog(st), &extHolder, nil, artifacts)

	taskMem, _ := newMemStoreForTest(t, "task")
	userMem, _ := newMemStoreForTest(t, "user")
	lgr := newLedgerStoreForTest()
	updateOrigin := newExtUpdateChatOrigin("noop", st, taskMem, userMem, lgr)

	const localID = "closes-unmerged"
	chatID := "ext:noop:" + localID
	origin := &extsdk.ChatOrigin{Extension: "noop", Label: "acme/widgets#9", Kind: "pull_request", Badge: "open", State: extsdk.SubjectOpen}
	req := extsdk.DispatchRequest{Chat: extsdk.ChatRef{LocalID: localID, Origin: origin}, Ask: extsdk.Ask{Message: "hi"}}
	if err := dispatch(context.Background(), req); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	waitRunSettled(t, st, chatID)

	seedMemory(t, taskMem, lgr, memory.Scope{Repo: "r"}, "repo", chatID, "a coding convention this run minted")
	seedMemory(t, userMem, lgr, memory.Scope{User: "u"}, "user", chatID, "a user fact this run minted")

	// Wired after seeding, so the fake only captures the outcome-feedback
	// rows this test asserts on, not the seed Commit's own ADD rows.
	ops := &fakeOpsLog{}
	taskMem.SetOpsLog(ops)
	userMem.SetOpsLog(ops)

	if err := updateOrigin(localID, extsdk.ChatOrigin{Extension: "noop", Label: "acme/widgets#9", Kind: "pull_request", Badge: "closed", State: extsdk.SubjectClosed}); err != nil {
		t.Fatalf("updateOrigin: %v", err)
	}

	taskMems := listAll(t, taskMem, []string{"repo:r"})
	if len(taskMems) != 1 || taskMems[0].Status != string(memory.StatusInvalidated) || taskMems[0].InvalidationReason != memory.OutcomeReasonClosedUnmerged {
		t.Fatalf("task memory = %+v, want status=invalidated reason=%q", taskMems, memory.OutcomeReasonClosedUnmerged)
	}
	userMems := listAll(t, userMem, []string{"user:u"})
	if len(userMems) != 1 || userMems[0].Status != string(memory.StatusInvalidated) || userMems[0].InvalidationReason != memory.OutcomeReasonClosedUnmerged {
		t.Fatalf("user memory = %+v, want status=invalidated reason=%q", userMems, memory.OutcomeReasonClosedUnmerged)
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

// TestUpdateChatOriginOpenToMergedReinforces: a transition to merged reinforces (count 0->1); a second
// update at the same State is a no-op.
func TestUpdateChatOriginOpenToMergedReinforces(t *testing.T) {
	st, orch, hub, artifacts, jail := newExtTestStack(t)
	_ = jail
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var extHolder atomic.Pointer[extsdk.Extension]
	dispatch := newExtDispatch("noop", &orchRef, st, hub, runlog.NewEventLog(st), &extHolder, nil, artifacts)

	taskMem, _ := newMemStoreForTest(t, "task")
	lgr := newLedgerStoreForTest()
	updateOrigin := newExtUpdateChatOrigin("noop", st, taskMem, nil, lgr)

	const localID = "merges-clean"
	chatID := "ext:noop:" + localID
	origin := &extsdk.ChatOrigin{Extension: "noop", Label: "acme/widgets#10", Kind: "pull_request", Badge: "open", State: extsdk.SubjectOpen}
	req := extsdk.DispatchRequest{Chat: extsdk.ChatRef{LocalID: localID, Origin: origin}, Ask: extsdk.Ask{Message: "hi"}}
	if err := dispatch(context.Background(), req); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	waitRunSettled(t, st, chatID)

	seedMemory(t, taskMem, lgr, memory.Scope{Repo: "r"}, "repo", chatID, "a repo convention that survived to merge")

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

// TestUpdateChatOriginRepeatedClosedAppliesNothing: closed -> closed (a repeat webhook) is steady state,
// so no outcome fires and no memory_ops row is written.
func TestUpdateChatOriginRepeatedClosedAppliesNothing(t *testing.T) {
	st, orch, hub, artifacts, jail := newExtTestStack(t)
	_ = jail
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var extHolder atomic.Pointer[extsdk.Extension]
	dispatch := newExtDispatch("noop", &orchRef, st, hub, runlog.NewEventLog(st), &extHolder, nil, artifacts)

	taskMem, _ := newMemStoreForTest(t, "task")
	lgr := newLedgerStoreForTest()
	updateOrigin := newExtUpdateChatOrigin("noop", st, taskMem, nil, lgr)

	const localID = "already-closed"
	chatID := "ext:noop:" + localID
	origin := &extsdk.ChatOrigin{Extension: "noop", Label: "acme/widgets#11", Kind: "issues", Badge: "closed", State: extsdk.SubjectClosed}
	req := extsdk.DispatchRequest{Chat: extsdk.ChatRef{LocalID: localID, Origin: origin}, Ask: extsdk.Ask{Message: "hi"}}
	if err := dispatch(context.Background(), req); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	waitRunSettled(t, st, chatID)

	seedMemory(t, taskMem, lgr, memory.Scope{Repo: "r"}, "repo", chatID, "a fact minted after this chat had already closed")

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

// TestUpdateChatOriginStatelessOriginAppliesNothing: State="" (an extension below sdk v0.5.0) is unknown,
// never a transition and never an error.
func TestUpdateChatOriginStatelessOriginAppliesNothing(t *testing.T) {
	st, orch, hub, artifacts, jail := newExtTestStack(t)
	_ = jail
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var extHolder atomic.Pointer[extsdk.Extension]
	dispatch := newExtDispatch("noop", &orchRef, st, hub, runlog.NewEventLog(st), &extHolder, nil, artifacts)

	taskMem, _ := newMemStoreForTest(t, "task")
	lgr := newLedgerStoreForTest()
	updateOrigin := newExtUpdateChatOrigin("noop", st, taskMem, nil, lgr)

	const localID = "stateless-origin"
	chatID := "ext:noop:" + localID
	origin := &extsdk.ChatOrigin{Extension: "noop", Label: "acme/widgets#12", Kind: "issues", Badge: "open"}
	req := extsdk.DispatchRequest{Chat: extsdk.ChatRef{LocalID: localID, Origin: origin}, Ask: extsdk.Ask{Message: "hi"}}
	if err := dispatch(context.Background(), req); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	waitRunSettled(t, st, chatID)

	seedMemory(t, taskMem, lgr, memory.Scope{Repo: "r"}, "repo", chatID, "a fact from a State-less extension version")

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

// TestUpdateChatOriginFollowsStateNotBadge: core never branches on Badge (display-only); Badge "open"
// with State closed must invalidate.
func TestUpdateChatOriginFollowsStateNotBadge(t *testing.T) {
	st, orch, hub, artifacts, jail := newExtTestStack(t)
	_ = jail
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var extHolder atomic.Pointer[extsdk.Extension]
	dispatch := newExtDispatch("noop", &orchRef, st, hub, runlog.NewEventLog(st), &extHolder, nil, artifacts)

	taskMem, _ := newMemStoreForTest(t, "task")
	ops := &fakeOpsLog{}
	taskMem.SetOpsLog(ops)
	lgr := newLedgerStoreForTest()
	updateOrigin := newExtUpdateChatOrigin("noop", st, taskMem, nil, lgr)

	const localID = "badge-lies"
	chatID := "ext:noop:" + localID
	origin := &extsdk.ChatOrigin{Extension: "noop", Label: "acme/widgets#13", Kind: "pull_request", Badge: "open", State: extsdk.SubjectOpen}
	req := extsdk.DispatchRequest{Chat: extsdk.ChatRef{LocalID: localID, Origin: origin}, Ask: extsdk.Ask{Message: "hi"}}
	if err := dispatch(context.Background(), req); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	waitRunSettled(t, st, chatID)

	seedMemory(t, taskMem, lgr, memory.Scope{Repo: "r"}, "repo", chatID, "a fact that must go by State, not Badge")

	// Badge still says "open" - only State moved to closed.
	if err := updateOrigin(localID, extsdk.ChatOrigin{Extension: "noop", Label: "acme/widgets#13", Kind: "pull_request", Badge: "open", State: extsdk.SubjectClosed}); err != nil {
		t.Fatalf("updateOrigin: %v", err)
	}

	mems := listAll(t, taskMem, []string{"repo:r"})
	if len(mems) != 1 || mems[0].Status != string(memory.StatusInvalidated) {
		t.Fatalf("memory = %+v, want status=invalidated (State overrides a stale Badge)", mems)
	}
}

// TestUpdateChatOriginSucceedsDespiteMemoryStoreFailure: applyMemoryOutcome logs and moves on, so the
// origin update succeeds with taskMem's table dropped and userMem nil.
func TestUpdateChatOriginSucceedsDespiteMemoryStoreFailure(t *testing.T) {
	st, orch, hub, artifacts, jail := newExtTestStack(t)
	_ = jail
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var extHolder atomic.Pointer[extsdk.Extension]
	dispatch := newExtDispatch("noop", &orchRef, st, hub, runlog.NewEventLog(st), &extHolder, nil, artifacts)

	taskMem, taskPath := newMemStoreForTest(t, "task")
	lgr := newLedgerStoreForTest()
	updateOrigin := newExtUpdateChatOrigin("noop", st, taskMem, nil, lgr) // userMem absent (nil)

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

// TestUpdateChatOriginReinforcesRecalledNotMinted: a merged outcome reinforces only memories recalled
// into the chat (memory.recall entry), not ones merely minted there.
func TestUpdateChatOriginReinforcesRecalledNotMinted(t *testing.T) {
	st, orch, hub, artifacts, jail := newExtTestStack(t)
	_ = jail
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var extHolder atomic.Pointer[extsdk.Extension]
	dispatch := newExtDispatch("noop", &orchRef, st, hub, runlog.NewEventLog(st), &extHolder, nil, artifacts)

	taskMem, _ := newMemStoreForTest(t, "task")
	lgr := newLedgerStoreForTest()
	updateOrigin := newExtUpdateChatOrigin("noop", st, taskMem, nil, lgr)

	const localID = "recall-vs-mint"
	chatID := "ext:noop:" + localID
	origin := &extsdk.ChatOrigin{Extension: "noop", Label: "acme/widgets#20", Kind: "pull_request", Badge: "open", State: extsdk.SubjectOpen}
	req := extsdk.DispatchRequest{Chat: extsdk.ChatRef{LocalID: localID, Origin: origin}, Ask: extsdk.Ask{Message: "hi"}}
	if err := dispatch(context.Background(), req); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	waitRunSettled(t, st, chatID)

	// Recalled: seedMemory mints AND appends a memory.recall entry.
	recalledID := seedMemory(t, taskMem, lgr, memory.Scope{Repo: "r"}, "repo", chatID, "a memory this run recalled and used")

	// Minted-only: no memory.recall entry for it - Commit directly, bypassing seedMemory.
	if _, err := taskMem.Commit(context.Background(), memory.Scope{Repo: "r"}, "test", memory.Provenance{ChatID: chatID},
		[]memory.Candidate{{Content: "a memory this run minted but never recalled"}}, ""); err != nil {
		t.Fatalf("commit minted-only: %v", err)
	}

	if err := updateOrigin(localID, extsdk.ChatOrigin{Extension: "noop", Label: "acme/widgets#20", Kind: "pull_request", Badge: "merged", State: extsdk.SubjectMerged}); err != nil {
		t.Fatalf("updateOrigin: %v", err)
	}

	mems := listAll(t, taskMem, []string{"repo:r"})
	byID := map[string]memory.Memory{}
	for _, m := range mems {
		byID[m.ID] = m
	}
	if g := byID[recalledID]; g.Status != string(memory.StatusReinforced) {
		t.Fatalf("recalled memory = %+v, want reinforced", g)
	}
	for id, m := range byID {
		if id == recalledID {
			continue
		}
		if m.Status == string(memory.StatusReinforced) {
			t.Fatalf("minted-only memory %+v was reinforced, want untouched (recall-based reinforcement, not birth-based)", m)
		}
	}
}
