package github

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
)

const explainChatID = "ext:github:github-acme-widgets-explain-alice-7"

// postedBody decodes the next comment the stub received, failing on none.
func postedBody(t *testing.T, posted <-chan string) string {
	t.Helper()
	select {
	case raw := <-posted:
		var c struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal([]byte(raw), &c); err != nil {
			t.Fatalf("decode posted comment %q: %v", raw, err)
		}
		return c.Body
	case <-time.After(2 * time.Second):
		t.Fatal("no comment posted")
		return ""
	}
}

func assertNothingPosted(t *testing.T, posted <-chan string) {
	t.Helper()
	select {
	case body := <-posted:
		t.Errorf("posted %q; want nothing", body)
	case <-time.After(200 * time.Millisecond):
	}
}

// startExplain sends one /explain webhook and consumes its link comment.
func startExplain(t *testing.T, publicURL string) (*Extension, *fakeDispatchHost, chan string, sdk.DispatchRequest, string) {
	t.Helper()
	posted := make(chan string, 4)
	srv := stubGitHub(t, posted)
	t.Cleanup(srv.Close)
	ext, fh := newTestExtension(t, srv.URL, []string{"explain"})
	ext.app.publicURL = publicURL
	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", reviewCommandBody(" /explain\n", "COLLABORATOR")))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d; want 202", rec.Code)
	}
	req := fh.waitForDispatch(t, 2*time.Second)
	return ext, fh, posted, req, postedBody(t, posted)
}

func TestHandleWebhookExplainCommand(t *testing.T) {
	_, _, _, req, link := startExplain(t, "https://quack.example")
	if req.Chat.LocalID != "github-acme-widgets-explain-alice-7" {
		t.Errorf("LocalID = %q; want a per-user explain session ending in the PR number", req.Chat.LocalID)
	}
	if !strings.HasPrefix(req.Chat.Title, "Explain: ") {
		t.Errorf("Title = %q; want an Explain: prefix", req.Chat.Title)
	}
	if k := req.Delivery.AllowedKinds; k == nil || len(k) != 0 {
		t.Errorf("AllowedKinds = %#v; want non-nil empty (deny every delivery)", k)
	}
	for _, ask := range []string{req.Ask.Message, req.Ask.NodeContext} {
		if !strings.Contains(ask, "<deliverable>Walk me through and quiz me on PR acme/widgets#7:") {
			t.Errorf("ask = %q; want the walkthrough-and-quiz deliverable", ask)
		}
	}
	if want := "Walkthrough and quiz for alice: https://quack.example/chat/" + explainChatID; link != want {
		t.Errorf("link comment = %q; want %q", link, want)
	}
}

func TestHandleWebhookExplainCommandNoPublicURL(t *testing.T) {
	_, _, _, _, link := startExplain(t, "")
	if want := "Walkthrough and quiz for alice: quack chat `" + explainChatID + "`"; link != want {
		t.Errorf("link comment = %q; want %q", link, want)
	}
}

func TestHandleWebhookExplainCommandIgnored(t *testing.T) {
	plainIssue := []byte(strings.Replace(string(reviewCommandBody("/explain", "OWNER")), `,"pull_request":{}`, "", 1))
	tests := []struct {
		name     string
		triggers []string
		body     []byte
	}{
		{"read-only author", []string{"explain"}, reviewCommandBody("/explain", "CONTRIBUTOR")},
		{"extra text is not the command", []string{"explain"}, reviewCommandBody("/explain please", "OWNER")},
		{"explain trigger disabled", []string{"mention", "label"}, reviewCommandBody("/explain", "OWNER", "quack-auto-review")},
		{"a plain issue", []string{"explain"}, plainIssue},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := stubGitHub(t, make(chan string, 1))
			defer srv.Close()
			ext, fh := newTestExtension(t, srv.URL, tt.triggers)
			ext.handleWebhook(httptest.NewRecorder(), signedRequest("issue_comment", tt.body))
			select {
			case <-fh.notify:
				t.Error("should not have dispatched a run")
			case <-time.After(200 * time.Millisecond):
			}
		})
	}
}

func TestHandleWebhookExplainCommandRespectsAllowlist(t *testing.T) {
	srv := stubGitHub(t, make(chan string, 1))
	defer srv.Close()
	ext, fh := newTestExtension(t, srv.URL, []string{"explain"})
	ext.allowedUsers = map[string]bool{"someone-else": true}

	rec := httptest.NewRecorder()
	ext.handleWebhook(rec, signedRequest("issue_comment", reviewCommandBody("/explain", "OWNER")))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rec.Code)
	}
	select {
	case <-fh.notify:
		t.Error("disallowed user should not dispatch")
	case <-time.After(200 * time.Millisecond):
	}
}

// A repeat while the first run is in flight re-posts the link, never a second run.
func TestExplainRepeatWhileRunningRepostsLink(t *testing.T) {
	ext, fh, posted, _, link := startExplain(t, "https://quack.example")
	ext.handleWebhook(httptest.NewRecorder(), signedRequest("issue_comment", reviewCommandBody("/explain", "OWNER")))
	if again := postedBody(t, posted); again != link {
		t.Errorf("repeat posted %q; want the same link %q", again, link)
	}
	if n := len(fh.calls()); n != 1 {
		t.Errorf("dispatches = %d; want 1", n)
	}
}

func TestExplainRunEndedPostsNothing(t *testing.T) {
	outcomes := []sdk.RunOutcome{
		{Status: sdk.RunDone, PlanRan: false, Answer: "walkthrough rendered"},
		{Status: sdk.RunNeedsInput, NodeID: "tutor", Question: "which file?"},
		{Status: sdk.RunFailed, Error: "boom"},
	}
	for _, o := range outcomes {
		t.Run(string(o.Status), func(t *testing.T) {
			ext, fh, posted, _, _ := startExplain(t, "")
			ext.RunEnded(explainChatID, o)
			assertNothingPosted(t, posted)
			if n := len(fh.calls()); n != 1 {
				t.Errorf("dispatches = %d; want 1 (an explain run is never nudged)", n)
			}
		})
	}
}

// After a restart only the durable row remains; the explain flag is rebuilt from its session id.
func TestExplainRunEndedAfterRestartPostsNothing(t *testing.T) {
	ext, _, posted, _, _ := startExplain(t, "")
	ext.pending.Delete(explainChatID)
	ext.RunEnded(explainChatID, sdk.RunOutcome{Status: sdk.RunDone, PlanRan: true, Answer: "walkthrough rendered"})
	assertNothingPosted(t, posted)
}

func TestAbortBlindExplain(t *testing.T) {
	posted := make(chan string, 1)
	srv := stubGitHub(t, posted)
	defer srv.Close()
	ext, _ := newTestExtension(t, srv.URL, []string{"explain"})
	p := issueCommentPayloadFor("acme", "widgets", 7, "alice", "/explain", "")
	p.explain = true
	if ext.abortBlind(p, githubContext{}, "acme", "widgets", 7) {
		t.Fatal("aborted with usable context")
	}
	if !ext.abortBlind(p, githubContext{contextUnavailable: true}, "acme", "widgets", 7) {
		t.Fatal("did not abort without GitHub context")
	}
	if body := postedBody(t, posted); !strings.Contains(body, "/explain again") {
		t.Errorf("abort comment = %q; want the /explain retry hint", body)
	}
}
