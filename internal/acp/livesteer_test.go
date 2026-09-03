package acp

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/workspace"
)

// TestRound_ForwardsLiveSteerIntoTheRunningSession (#998): a message handed
// to the captured forward func reaches the still-running Prompt RPC. Keys
// come straight from round()'s own steerChatID/steerNodeID params - not
// a.coords (the shared, racy field a real caller must never key the hook on;
// see acp.go's resolveNode/runPrompt, which resolve them per-round from the
// advisor thread).
func TestRound_ForwardsLiveSteerIntoTheRunningSession(t *testing.T) {
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	var gotChat, gotNode string
	var forward func(string) bool
	registered := make(chan struct{})
	unregistered := make(chan struct{})

	a, err := New("code-implementer", "external coder", Options{
		Command: []string{os.Args[0]},
		Env:     []string{"QUACK_ACP_FAKE=steer"},
		Home:    t.TempDir(),
		Jail:    jail,
		UserID:  "u1",
		RegisterLiveSteer: func(chatID, nodeID string, f func(string) bool) {
			mu.Lock()
			gotChat, gotNode, forward = chatID, nodeID, f
			mu.Unlock()
			close(registered)
		},
		UnregisterLiveSteer: func(chatID, nodeID string) { close(unregistered) },
	})
	if err != nil {
		t.Fatal(err)
	}

	var specs []eventSpec
	done := make(chan error, 1)
	go func() {
		done <- a.round(context.Background(), t.TempDir(), "", nil, workspace.Caps{}, "add the feature", "chat1", "n1", "", "", func(s eventSpec) bool {
			specs = append(specs, s)
			return true
		})
	}()

	<-registered
	mu.Lock()
	chatID, nodeID, fwd := gotChat, gotNode, forward
	mu.Unlock()
	if chatID != "chat1" || nodeID != "n1" {
		t.Fatalf("RegisterLiveSteer got (%q,%q), want (chat1,n1)", chatID, nodeID)
	}
	if !fwd("focus on cost") {
		t.Fatal("forward reported failure delivering into the live round")
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("round: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("round never returned after the steer was forwarded")
	}
	select {
	case <-unregistered:
	case <-time.After(2 * time.Second):
		t.Fatal("UnregisterLiveSteer never called")
	}

	if len(specs) == 0 || specs[len(specs)-1].parts[0].Text != "steered: focus on cost" {
		t.Fatalf("round never saw the forwarded steer; final spec = %+v", specs)
	}
}

// TestRound_SteerRejectedByShimReportsFailure (#998 review): the forward
// func must use an ACKED call (CallExtension), not a fire-and-forget
// notification - otherwise a steer landing in the window between the shim
// settling the round and quack's deferred Unregister would report delivered
// while silently dropped. The "steer-reject" fake agent errors every
// _quack/steer call (as the shim does once promptReq is already nil) - the
// forward func must surface that as false so enqueue's caller parks instead
// of marking the message delivered.
func TestRound_SteerRejectedByShimReportsFailure(t *testing.T) {
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	var forward func(string) bool
	registered := make(chan struct{})

	a, err := New("code-implementer", "external coder", Options{
		Command: []string{os.Args[0]},
		Env:     []string{"QUACK_ACP_FAKE=steer-reject"},
		Home:    t.TempDir(),
		Jail:    jail,
		UserID:  "u1",
		RegisterLiveSteer: func(chatID, nodeID string, f func(string) bool) {
			forward = f
			close(registered)
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		done <- a.round(context.Background(), t.TempDir(), "", nil, workspace.Caps{}, "add the feature", "chat1", "n1", "", "", func(eventSpec) bool { return true })
	}()

	<-registered
	if forward("too late") {
		t.Fatal("forward reported success for a steer the shim rejected - a caller would wrongly mark it delivered")
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("round: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("round never finished")
	}
}
