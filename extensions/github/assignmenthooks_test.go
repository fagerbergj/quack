package github

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/fagerbergj/quack-extensions/sdk"
)

// fakeSessionCtx stands in for quack's agent.Context: OnAssignment recovers the chat id
// via a structural SessionID() method, not a concrete adk type.
type fakeSessionCtx struct {
	context.Context
	sessionID string
}

func (f fakeSessionCtx) SessionID() string { return f.sessionID }

// refHandler serves the installation/token dance plus one git ref lookup,
// returning sha for any heads/<branch> GET.
func refHandler(t *testing.T, sha string) (*httptest.Server, *int32) {
	t.Helper()
	var refHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprint(w, `{"token":"ghs_x","expires_at":"2099-01-01T00:00:00Z"}`)
		case strings.Contains(r.URL.Path, "/git/ref/heads/"):
			refHits++
			fmt.Fprintf(w, `{"object":{"sha":%q}}`, sha)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	return srv, &refHits
}

func newAssignmentTestExtension(t *testing.T, apiBase string) *Extension {
	t.Helper()
	app := newTestApp(t, apiBase)
	return &Extension{app: app, host: sdk.Host{Log: slog.Default()}, pending: sync.Map{}}
}

func TestOnAssignment_StampsOwnDispatch(t *testing.T) {
	srv, _ := refHandler(t, "cafef00d1234567890")
	defer srv.Close()
	e := newAssignmentTestExtension(t, srv.URL)
	e.pending.Store("chat1", &pendingRun{
		owner: "acme", repo: "widgets",
		dispatched: sdk.DispatchRequest{Run: sdk.RunConfig{Setup: &sdk.Setup{
			Repo: "https://github.com/acme/widgets.git", BaseRef: "main",
		}}},
	})

	got := e.OnAssignment(fakeSessionCtx{context.Background(), "chat1"}, sdk.Assignment{NodeID: "impl-1"})
	want := map[string]any{"repo": "https://github.com/acme/widgets.git", "base_ref": "main", "base_sha": "cafef00d1234567890"}
	if got["repo"] != want["repo"] || got["base_ref"] != want["base_ref"] || got["base_sha"] != want["base_sha"] {
		t.Errorf("OnAssignment = %+v, want %+v", got, want)
	}
}

// A PR-anchored dispatch tracks the PR's own head branch, not BaseRef.
func TestOnAssignment_PRDispatchTracksExistingHeadRef(t *testing.T) {
	srv, _ := refHandler(t, "deadbeef00")
	defer srv.Close()
	e := newAssignmentTestExtension(t, srv.URL)
	e.pending.Store("chat1", &pendingRun{
		owner: "acme", repo: "widgets",
		dispatched: sdk.DispatchRequest{Run: sdk.RunConfig{Setup: &sdk.Setup{
			Repo: "https://github.com/acme/widgets.git", BaseRef: "main", ExistingHeadRef: "pr-42-head",
		}}},
	})

	got := e.OnAssignment(fakeSessionCtx{context.Background(), "chat1"}, sdk.Assignment{NodeID: "impl-1"})
	if got["base_ref"] != "pr-42-head" {
		t.Errorf("base_ref = %v, want the PR's own head branch", got["base_ref"])
	}
}

func TestOnAssignment_NilForAnyOtherChat(t *testing.T) {
	srv, refHits := refHandler(t, "unused")
	defer srv.Close()
	e := newAssignmentTestExtension(t, srv.URL)
	e.pending.Store("chat1", &pendingRun{
		owner: "acme", repo: "widgets",
		dispatched: sdk.DispatchRequest{Run: sdk.RunConfig{Setup: &sdk.Setup{Repo: "x", BaseRef: "main"}}},
	})

	if got := e.OnAssignment(fakeSessionCtx{context.Background(), "chat-unrelated"}, sdk.Assignment{}); got != nil {
		t.Errorf("OnAssignment for an untracked chat = %+v, want nil", got)
	}
	// A plain context.Context (no SessionID method) - the same "not mine" answer.
	if got := e.OnAssignment(context.Background(), sdk.Assignment{}); got != nil {
		t.Errorf("OnAssignment with no session id = %+v, want nil", got)
	}
	if *refHits != 0 {
		t.Errorf("git ref endpoint hit %d times for a chat that isn't this extension's dispatch; want 0", *refHits)
	}
}

func TestBeforeAssignment_Fresh(t *testing.T) {
	srv, _ := refHandler(t, "cafef00d1234567890")
	defer srv.Close()
	e := newAssignmentTestExtension(t, srv.URL)

	a := sdk.Assignment{Meta: map[string]map[string]any{"github": {
		"repo": "https://github.com/acme/widgets.git", "base_ref": "main", "base_sha": "cafef00d1234567890",
	}}}
	fresh, reason := e.BeforeAssignment(context.Background(), a)
	if !fresh || reason != "" {
		t.Errorf("BeforeAssignment = (%v, %q), want (true, \"\")", fresh, reason)
	}
}

func TestBeforeAssignment_Stale(t *testing.T) {
	srv, _ := refHandler(t, "newtip9999999999999")
	defer srv.Close()
	e := newAssignmentTestExtension(t, srv.URL)

	a := sdk.Assignment{Meta: map[string]map[string]any{"github": {
		"repo": "https://github.com/acme/widgets.git", "base_ref": "main", "base_sha": "oldtip0000000000000",
	}}}
	fresh, reason := e.BeforeAssignment(context.Background(), a)
	if fresh {
		t.Fatal("want stale (base moved)")
	}
	if want := "base moved: oldtip0 -> newtip9"; reason != want {
		t.Errorf("reason = %q, want %q", reason, want)
	}
}

func TestBeforeAssignment_APIErrorFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprint(w, `{"token":"ghs_x","expires_at":"2099-01-01T00:00:00Z"}`)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()
	e := newAssignmentTestExtension(t, srv.URL)

	a := sdk.Assignment{Meta: map[string]map[string]any{"github": {
		"repo": "https://github.com/acme/widgets.git", "base_ref": "main", "base_sha": "cafef00d1234567890",
	}}}
	fresh, reason := e.BeforeAssignment(context.Background(), a)
	if fresh {
		t.Fatal("want fail-closed to stale on an API error")
	}
	if !strings.HasPrefix(reason, "could not check tip: ") {
		t.Errorf("reason = %q, want it to start with %q", reason, "could not check tip: ")
	}
}

func TestBeforeAssignment_NoGithubMetaIsNotThisExtensionsConcern(t *testing.T) {
	srv, refHits := refHandler(t, "unused")
	defer srv.Close()
	e := newAssignmentTestExtension(t, srv.URL)

	fresh, reason := e.BeforeAssignment(context.Background(), sdk.Assignment{Meta: map[string]map[string]any{"acme": {"ticket": "ACME-1"}}})
	if !fresh || reason != "" {
		t.Errorf("BeforeAssignment with no github meta = (%v, %q), want (true, \"\")", fresh, reason)
	}
	if *refHits != 0 {
		t.Errorf("git ref endpoint hit %d times for an assignment with no github meta; want 0", *refHits)
	}
}
