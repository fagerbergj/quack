package github

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fagerbergj/quack/extensions/sdk"
)

var errBusy = fmt.Errorf("extensions.github: chat %w", sdk.ErrRunInFlight)

// seedLiveRun leaves chat 7 held by a run past its claim lease, with the records RunEnded needs.
func seedLiveRun(t *testing.T, e *Extension) (chatID, sessionID string, live *pendingRun) {
	t.Helper()
	sessionID = "github-acme-widgets-7"
	chatID = globalChatID(sessionID)
	live = &pendingRun{
		sessionID: sessionID, claimedAt: time.Now().Add(-e.inflightLease() - time.Minute),
		owner: "acme", repo: "widgets", number: 7, login: "alice",
	}
	e.inflight.Store(sessionID, live.claimedAt)
	e.pending.Store(chatID, live)
	row := PendingRunRow{ChatID: chatID, SessionID: sessionID, Owner: "acme", Repo: "widgets", Number: 7, Login: "alice"}
	if err := e.store.SetPendingRun(context.Background(), row); err != nil {
		t.Fatalf("SetPendingRun: %v", err)
	}
	return chatID, sessionID, live
}

func waitPosted(t *testing.T, posted <-chan string) string {
	t.Helper()
	select {
	case body := <-posted:
		return body
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted")
		return ""
	}
}

// A trigger refused busy must leave the live run's record alone, so that run's outcome still reaches GitHub.
func TestBusyDispatchKeepsLiveRunsRecord(t *testing.T) {
	posted := make(chan string, 4)
	srv, reactions := stubGitHubCounting(t, posted)
	defer srv.Close()
	e, fh := newTestExtension(t, srv.URL, nil)
	chatID, sessionID, live := seedLiveRun(t, e)
	fh.dispatchErr = errBusy

	// A review trigger: had its record replaced the live one, the live run would post a no-verdict note.
	e.dispatch(issueCommentPayloadFor("acme", "widgets", 7, "bob", "/review", srv.URL), autoReviewTask)
	e.Wait()

	// dispatch itself never acks (handleWebhook does), so the busy branch's ackDuplicate is the only 👀.
	if got := reactions.Load(); got != 1 {
		t.Errorf("reaction POSTs = %d; want the busy refusal's single 👀 ack", got)
	}

	if v, ok := e.pending.Load(chatID); !ok || v.(*pendingRun) != live {
		t.Fatalf("pending = %v, %v; want the live run's record untouched", v, ok)
	}
	if row, err := e.store.GetPendingRun(context.Background(), chatID); err != nil || row == nil || row.Login != "alice" {
		t.Fatalf("pending row = %+v, %v; want the live run's row untouched", row, err)
	}
	if _, ok := e.inflight.Load(sessionID); ok {
		t.Error("a busy refusal kept its claim; the next trigger after the live run ends would be dropped")
	}

	e.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "the live run's answer"})
	if body := waitPosted(t, posted); !strings.Contains(body, "the live run's answer") {
		t.Errorf("posted = %q; want the live run's answer", body)
	}
	if row, _ := e.store.GetPendingRun(context.Background(), chatID); row != nil {
		t.Errorf("pending row = %+v after the live run finalized; want it deleted", row)
	}
}

// RunEnded racing Dispatch reads only a settled record: the live run's while a trigger is refused busy,
// and the new run's own once Dispatch accepts it, even when that run ends before Dispatch returns.
func TestRunEndedDuringDispatchSeesSettledRecord(t *testing.T) {
	for _, tc := range []struct {
		name       string
		busy       bool
		wantAnswer string
	}{
		{"refused busy: the live run's record", true, "the live run's answer"},
		{"accepted: the new run's record", false, "Review finished without a verdict"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			posted := make(chan string, 4)
			srv := stubGitHub(t, posted)
			defer srv.Close()
			e, _ := newTestExtension(t, srv.URL, nil)
			chatID, sessionID := globalChatID("github-acme-widgets-7"), "github-acme-widgets-7"
			if tc.busy {
				seedLiveRun(t, e)
			}
			ended := make(chan struct{})
			e.host.Dispatch = func(context.Context, sdk.DispatchRequest) error {
				go func() {
					defer close(ended)
					e.RunEnded(chatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "the live run's answer"})
				}()
				// Give RunEnded time to reach its lookup while Dispatch is still deciding.
				time.Sleep(50 * time.Millisecond)
				if tc.busy {
					return errBusy
				}
				return nil
			}

			e.dispatch(issueCommentPayloadFor("acme", "widgets", 7, "bob", "/review", srv.URL), autoReviewTask)
			<-ended
			e.Wait()

			if body := waitPosted(t, posted); !strings.Contains(body, tc.wantAnswer) {
				t.Errorf("posted = %q; want %q", body, tc.wantAnswer)
			}
			if _, ok := e.pending.Load(chatID); ok {
				t.Error("pending record left behind after its run finalized")
			}
			if _, ok := e.inflight.Load(sessionID); ok {
				t.Error("inflight claim left behind")
			}
		})
	}
}

// A finished run's late finalize must not delete the record of a run dispatched onto the chat after it.
func TestFinalizeKeepsNewerPendingRecord(t *testing.T) {
	e, _ := newTestExtension(t, "http://127.0.0.1:0", nil)
	chatID := globalChatID("github-acme-widgets-7")
	older := &pendingRun{sessionID: "github-acme-widgets-7", owner: "acme", repo: "widgets", number: 7}
	newer := &pendingRun{sessionID: "github-acme-widgets-7", owner: "acme", repo: "widgets", number: 7}
	e.pending.Store(chatID, newer)

	e.finalize(chatID, older, sdk.RunOutcome{Status: sdk.RunCancelled})

	if v, ok := e.pending.Load(chatID); !ok || v.(*pendingRun) != newer {
		t.Errorf("pending = %v, %v; want the newer run's record kept", v, ok)
	}
}
