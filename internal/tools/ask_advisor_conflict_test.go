package tools

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/store"
)

// TestConsultAdvisor_ConcurrentSameThreadDBService pins the live 2026-07-09
// stale-session fix: ADK executes a model turn's function calls in CONCURRENT
// goroutines (llminternal/base_flow.go handleFunctionCalls), so two
// ask_advisor calls for the SAME thread can run at once. Each consult spins
// its own runner lifecycle (Get/Create → append) holding its own localSession
// snapshot of the one advisor session row; the database service's optimistic
// stale check (session/database/service.go applyEvent) rejects the loser's
// append ("stale session error"). InMemoryService has no such check — which
// is why the original tests missed it; this test runs against the REAL
// database-backed service (sqlite dialect of the production Postgres one).
//
// With the per-thread serialization + stale retry in consultAdvisor, every
// concurrent consult must succeed and all requests must land in the ONE
// advisor session, in some order.
func TestConsultAdvisor_ConcurrentSameThreadDBService(t *testing.T) {
	st, err := store.New("sqlite", filepath.Join(t.TempDir(), "quack.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	advisorAgent, err := llmagent.New(llmagent.Config{
		Name: "advisor", Model: &recordingAdvisor{}, Description: "advisor", Instruction: "Advise.",
		Mode: llmagent.ModeChat,
	})
	if err != nil {
		t.Fatalf("advisor agent: %v", err)
	}

	const n = 4
	const token = "p1/n1"
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = consultAdvisor(context.Background(), advisorAgent, st.Sessions,
				token, "SEED: the task", fmt.Sprintf("REQ-%d", i))
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Errorf("consult %d failed: %v", i, e)
		}
	}

	// Every request must be in the single advisor session's history.
	resp, err := st.Sessions.Get(context.Background(), &session.GetRequest{
		AppName: advisorAppName, UserID: advisorUserID, SessionID: token + ":advisor",
	})
	if err != nil {
		t.Fatalf("get advisor session: %v", err)
	}
	var all strings.Builder
	for ev := range resp.Session.Events().All() {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && p.Text != "" {
				all.WriteString(p.Text)
				all.WriteByte('\n')
			}
		}
	}
	for i := 0; i < n; i++ {
		if !strings.Contains(all.String(), fmt.Sprintf("REQ-%d", i)) {
			t.Errorf("advisor session missing REQ-%d (a concurrent consult was lost)", i)
		}
	}
	// The seed must appear EXACTLY once — the first consult to create the
	// thread seeds it; retried/serialized consults must not re-seed.
	if got := strings.Count(all.String(), "SEED: the task"); got != 1 {
		t.Errorf("seed appears %d times in the advisor session, want exactly 1", got)
	}
}

// TestIsSessionConflict pins the transient-conflict detection consultAdvisor
// retries on: the database service's stale-session error and both databases'
// unique-violation wordings for the create race. Anything else must NOT be
// retried (immediate graceful degradation).
func TestIsSessionConflict(t *testing.T) {
	stale := fmt.Errorf("failed to add event to session: %w",
		errors.New("stale session error: last update time from request (x) is older than in database (y)"))
	if !isSessionConflict(stale) {
		t.Error("wrapped stale session error not detected")
	}
	if !isSessionConflict(errors.New("error creating session on database: constraint failed: UNIQUE constraint failed: sessions.app_name (1555)")) {
		t.Error("sqlite unique violation not detected")
	}
	if !isSessionConflict(errors.New(`error creating session on database: ERROR: duplicate key value violates unique constraint "sessions_pkey" (SQLSTATE 23505)`)) {
		t.Error("postgres unique violation not detected")
	}
	if isSessionConflict(errors.New("model timeout")) {
		t.Error("unrelated error misdetected as a session conflict")
	}
	if isSessionConflict(nil) {
		t.Error("nil misdetected")
	}
}
