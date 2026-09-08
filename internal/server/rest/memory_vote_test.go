package rest

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/schema"
)

// fakeLedgerStore is an in-memory ledger.LedgerStore for tests that need
// AppendIntent/ReadEntries without a real Postgres backend (unlike
// failingLedgerStore in errors_test.go, this one actually stores entries).
type fakeLedgerStore struct {
	mu      sync.Mutex
	entries []ledger.Entry
	nextSeq int64
}

func (f *fakeLedgerStore) AppendIntent(_ context.Context, e ledger.Entry) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextSeq++
	e.Seq = f.nextSeq
	f.entries = append(f.entries, e)
	return e.Seq, nil
}

func (f *fakeLedgerStore) ReadEntries(_ context.Context, chatID string, fromSeq int64) ([]ledger.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []ledger.Entry
	for _, e := range f.entries {
		if e.ChatID == chatID && e.Seq >= fromSeq {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeLedgerStore) MaxSeq(_ context.Context, chatID string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var max int64
	for _, e := range f.entries {
		if e.ChatID == chatID && e.Seq > max {
			max = e.Seq
		}
	}
	return max, nil
}

func (f *fakeLedgerStore) List(context.Context) ([]ledger.SessionRef, error) { return nil, nil }
func (f *fakeLedgerStore) Delete(context.Context, string) error              { return nil }

// TestVoteMemory_UpThenNoneRoundTrip covers epic #1255 P4: an up vote raises
// upvotes/tier/own_vote, appends one memory.vote ledger entry, and voting
// "none" (the toggle-off) removes it again.
func TestVoteMemory_UpThenNoneRoundTrip(t *testing.T) {
	h := newTestHandler(t)
	h.taskMem = newTestMemStore(t)
	h.ledgerStore = &fakeLedgerStore{}
	commitFact(t, h.taskMem, "NightsOut", "the deploy script needs sudo")

	mems, _, err := h.taskMem.List(context.Background(), nil, 0, 10, false)
	if err != nil || len(mems) != 1 {
		t.Fatalf("seed list: %v, %d mems", err, len(mems))
	}
	id := mems[0].ID

	vote := func(v string) *schema.Memory {
		body, _ := json.Marshal(schema.VoteMemoryBody{Vote: schema.VoteMemoryBodyVote(v)})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/memories/"+id+"/vote", bytes.NewReader(body))
		h.VoteMemory(rec, req, id)
		if rec.Code != http.StatusOK {
			t.Fatalf("VoteMemory(%q) status=%d body=%s", v, rec.Code, rec.Body.String())
		}
		var out schema.Memory
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return &out
	}

	up := vote("up")
	if up.Upvotes == nil || *up.Upvotes != 1 || up.OwnVote == nil || *up.OwnVote != schema.MemoryOwnVoteUp {
		t.Fatalf("after up: %+v", up)
	}

	chatKey := mems[0].ChatID
	if chatKey == "" {
		chatKey = humanVoteChatKey // commitFact's Provenance{} leaves ChatID empty
	}
	entries, err := h.ledgerStore.ReadEntries(context.Background(), chatKey, 0)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	if len(entries) != 1 || entries[0].Kind != ledger.KindMemoryVote {
		t.Fatalf("ledger entries = %+v, want exactly one memory.vote", entries)
	}

	none := vote("none")
	if none.Upvotes == nil || *none.Upvotes != 0 || none.OwnVote != nil {
		t.Fatalf("after none: %+v, want upvotes=0 own_vote absent", none)
	}
}

// TestVoteMemory_UnknownID404 mirrors DeleteMemory's 404 contract.
func TestVoteMemory_UnknownID404(t *testing.T) {
	h := newTestHandler(t)
	h.taskMem = newTestMemStore(t)
	body, _ := json.Marshal(schema.VoteMemoryBody{Vote: schema.Up})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/memories/no-such-id/vote", bytes.NewReader(body))
	h.VoteMemory(rec, req, "no-such-id")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

// TestListNodeMemories_FoldsRecallAndVote covers the P4 node-memories read:
// a memory.recall entry for a node surfaces with its source, and a
// memory.vote entry for the same node/memory attaches the vote+reason.
func TestListNodeMemories_FoldsRecallAndVote(t *testing.T) {
	h := newTestHandler(t)
	h.taskMem = newTestMemStore(t)
	h.ledgerStore = &fakeLedgerStore{}
	commitFact(t, h.taskMem, "NightsOut", "retry uploads on 5xx")
	mems, _, err := h.taskMem.List(context.Background(), nil, 0, 10, false)
	if err != nil || len(mems) != 1 {
		t.Fatalf("seed list: %v, %d mems", err, len(mems))
	}
	id := mems[0].ID
	chatID, nodeID := "chat-1", "node-1"

	recallPayload, _ := json.Marshal(ledger.MemoryRecallPayload{
		Source: "prefill", Round: 1, Entries: []ledger.MemoryRecallEntry{{ID: id, Score: 0.9}},
	})
	if _, err := h.ledgerStore.AppendIntent(context.Background(), ledger.Entry{
		ChatID: chatID, NodeID: nodeID, Kind: ledger.KindMemoryRecall, Payload: recallPayload,
	}); err != nil {
		t.Fatalf("append recall: %v", err)
	}
	votePayload, _ := json.Marshal(ledger.MemoryVotePayload{MemoryID: id, Vote: ledger.MemoryVoteSupported, Reason: "matched the fix", Actor: "judge"})
	if _, err := h.ledgerStore.AppendIntent(context.Background(), ledger.Entry{
		ChatID: chatID, NodeID: nodeID, Kind: ledger.KindMemoryVote, Payload: votePayload,
	}); err != nil {
		t.Fatalf("append vote: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats/"+chatID+"/nodes/"+nodeID+"/memories", nil)
	h.ListNodeMemories(rec, req, chatID, nodeID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var out schema.NodeMemoryList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Memories) != 1 {
		t.Fatalf("memories = %+v, want exactly 1", out.Memories)
	}
	nm := out.Memories[0]
	if nm.Id != id || nm.Source != schema.Prefill || nm.Vote == nil || *nm.Vote != schema.Supported || nm.Reason == nil || *nm.Reason != "matched the fix" {
		t.Fatalf("node memory = %+v, want id=%s source=prefill vote=supported reason set", nm, id)
	}
}

// TestListNodeMemories_NoLedger_Empty: a server with no ledger store returns
// an empty list, not a 500 - the endpoint degrades gracefully like
// GetChatRecording's ledgerStore-nil path elsewhere in this package.
func TestListNodeMemories_NoLedger_Empty(t *testing.T) {
	h := newTestHandler(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/chats/c/nodes/n/memories", nil)
	h.ListNodeMemories(rec, req, "c", "n")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var out schema.NodeMemoryList
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Memories) != 0 {
		t.Fatalf("memories = %+v, want empty", out.Memories)
	}
}
