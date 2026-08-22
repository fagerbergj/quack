package acp

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/workspace"
)

// TestRound_ForwardsLiveSteerIntoTheRunningSession (#998): a message handed
// to the captured forward func reaches the still-running Prompt RPC.
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

	// round() reads coords from a.coords (SetLedgerCoords), not from ctx -
	// that's the seam RegisterLiveSteer keys chatID/nodeID off of.
	a.SetLedgerCoords(ledger.Coords{ChatID: "chat1", Node: "n1"})
	var specs []eventSpec
	done := make(chan error, 1)
	go func() {
		done <- a.round(context.Background(), t.TempDir(), "", nil, workspace.Caps{}, "add the feature", func(s eventSpec) bool {
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
