package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fagerbergj/quack-extensions/sdk"
)

// newDeliveryApp wires an App to an httptest server with install/token caches pre-seeded,
// so only the stubbed endpoints are hit.
func newDeliveryApp(t *testing.T, handler http.HandlerFunc) *App {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	keyPEM, _ := testKeyPEM(t)
	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.apiBase = srv.URL
	app.installs["acme/widgets"] = 1
	app.tokens[1] = cachedToken{token: "ghs_x", expires: time.Now().Add(time.Hour)}
	return app
}

// A staged pull_request for a branch that already has an OPEN PR updates it, never opens a second.
func TestDeliverPullRequestUpdatesExistingInsteadOfDuplicate(t *testing.T) {
	var created bool
	var patchedBody map[string]string
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
			if !strings.Contains(r.URL.RawQuery, "head=acme:feature") || !strings.Contains(r.URL.RawQuery, "state=open") {
				t.Errorf("findOpenPR query = %q; want head=acme:feature&state=open", r.URL.RawQuery)
			}
			io.WriteString(w, `[{"number":42,"html_url":"https://github.com/acme/widgets/pull/42"}]`)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/pulls/42"):
			data, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(data, &patchedBody)
			io.WriteString(w, `{"html_url":"https://github.com/acme/widgets/pull/42"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			created = true
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"html_url":"https://github.com/acme/widgets/pull/99","number":99}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	dc := sdk.DeliveryContext{
		GatePassed: true, // these test delivery mechanics, not the gate caveat
		ChatID:     "chat-1",
		CloneURL:   "https://github.com/acme/widgets.git",
		Branch:     "feature", // PushedSHA empty ⇒ no push verification attempted
		Items:      []sdk.StagedDelivery{{Kind: "pull_request", Title: "Add widget", Body: "does the thing"}},
	}
	if _, err := app.Deliver(context.Background(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if created {
		t.Error("a NEW pull request was opened despite an existing open PR for this branch")
	}
	if patchedBody["title"] != "Add widget" || patchedBody["body"] != "does the thing" {
		t.Errorf("existing PR was not updated with the staged title/body; patch body = %+v", patchedBody)
	}
	d, ok := takeDeliveryDetail("chat-1")
	if !ok || d.err != nil || d.prNumber != 42 {
		t.Errorf("recorded outcome = %+v, ok=%v; want the VERIFIED existing pr_number 42, no error", d, ok)
	}
}

// A gate push failure on dc.PushError posts the failure and attempts nothing against the unpushed branch.
func TestDeliverPushErrorShortCircuits(t *testing.T) {
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request %s %s - Deliver must not call GitHub on a push failure", r.Method, r.URL.Path)
	})
	dc := sdk.DeliveryContext{
		ChatID:    "chat-1",
		CloneURL:  "https://github.com/acme/widgets.git",
		Branch:    "feature",
		PushError: "git push exit 128",
		Items:     []sdk.StagedDelivery{{Kind: "pull_request", Title: "Add widget", Body: "does the thing"}},
	}
	outcomes, err := app.Deliver(context.Background(), dc)
	if err == nil || !strings.Contains(err.Error(), "git push exit 128") {
		t.Fatalf("Deliver err = %v, want it naming the push failure", err)
	}
	if len(outcomes) != 1 || outcomes[0].Error == "" {
		t.Fatalf("outcomes = %+v, want every item marked failed", outcomes)
	}
}

// A stage_push with no title/body leaves the PR untouched: no PATCH request at all.
func TestDeliverPushWithNothingToSayOmitsThePatch(t *testing.T) {
	var patched bool
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
			io.WriteString(w, `[{"number":42,"html_url":"https://github.com/acme/widgets/pull/42"}]`)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/pulls/42"):
			patched = true
			io.WriteString(w, `{"html_url":"https://github.com/acme/widgets/pull/42"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	dc := sdk.DeliveryContext{
		GatePassed: true,
		ChatID:     "chat-724-1",
		CloneURL:   "https://github.com/acme/widgets.git",
		Branch:     "feature",
		Items:      []sdk.StagedDelivery{{Kind: "pull_request", TitleOmitted: true, BodyOmitted: true}},
	}
	outcomes, err := app.Deliver(context.Background(), dc)
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if patched {
		t.Error("stage_push with nothing to say must not PATCH the pull request at all")
	}
	if len(outcomes) != 1 || outcomes[0].Error != "" || outcomes[0].URL != "https://github.com/acme/widgets/pull/42" {
		t.Errorf("outcomes = %+v, want a clean success against pull 42", outcomes)
	}
}

// stage_push(title, body) applies both fields.
func TestDeliverPushWithDeliberateUpdateAppliesBoth(t *testing.T) {
	var patchedBody map[string]any
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
			io.WriteString(w, `[{"number":42,"html_url":"https://github.com/acme/widgets/pull/42"}]`)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/pulls/42"):
			data, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(data, &patchedBody)
			io.WriteString(w, `{"html_url":"https://github.com/acme/widgets/pull/42"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	dc := sdk.DeliveryContext{
		GatePassed: true,
		ChatID:     "chat-724-2",
		CloneURL:   "https://github.com/acme/widgets.git",
		Branch:     "feature",
		Items:      []sdk.StagedDelivery{{Kind: "pull_request", Title: "fix: retry flaky upload", Body: "adds a backoff"}},
	}
	if _, err := app.Deliver(context.Background(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if len(patchedBody) != 2 || patchedBody["title"] != "fix: retry flaky upload" || patchedBody["body"] != "adds a backoff" {
		t.Errorf("PATCH body = %+v, want exactly title and body set", patchedBody)
	}
}

// A body-only stage_push PATCHes with no "title" key; an empty string would blank the PR's real title.
func TestDeliverPushPartialUpdateOmitsTitleKey(t *testing.T) {
	var patchedBody map[string]any
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
			io.WriteString(w, `[{"number":42,"html_url":"https://github.com/acme/widgets/pull/42"}]`)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/pulls/42"):
			data, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(data, &patchedBody)
			io.WriteString(w, `{"html_url":"https://github.com/acme/widgets/pull/42"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	dc := sdk.DeliveryContext{
		GatePassed: true,
		ChatID:     "chat-724-3",
		CloneURL:   "https://github.com/acme/widgets.git",
		Branch:     "feature",
		Items:      []sdk.StagedDelivery{{Kind: "pull_request", Body: "adds a backoff and jitter", TitleOmitted: true}},
	}
	if _, err := app.Deliver(context.Background(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if _, hasTitle := patchedBody["title"]; hasTitle {
		t.Errorf("PATCH body carried a title key for an omitted title: %+v", patchedBody)
	}
	if got, hasBody := patchedBody["body"]; !hasBody || got != "adds a backoff and jitter" {
		t.Errorf("PATCH body missing/wrong body key: %+v", patchedBody)
	}
}

// A titleless stage_push with no open PR for the branch must refuse with a named cause, never invent
// a title or call the create-PR endpoint (GitHub would 422 a titleless PR).
func TestDeliverPushWithNoTitleAndNoExistingPRFailsExplicitly(t *testing.T) {
	var posted bool
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
			io.WriteString(w, `[]`) // no open PR for this branch anymore
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			posted = true
			w.WriteHeader(http.StatusUnprocessableEntity)
			io.WriteString(w, `{"message":"Validation Failed"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	dc := sdk.DeliveryContext{
		GatePassed: true,
		ChatID:     "chat-724-4",
		CloneURL:   "https://github.com/acme/widgets.git",
		Branch:     "feature",
		Items:      []sdk.StagedDelivery{{Kind: "pull_request", TitleOmitted: true, BodyOmitted: true}},
	}
	outcomes, err := app.Deliver(context.Background(), dc)
	if err == nil {
		t.Fatal("Deliver: want an error - a titleless push with no PR to push onto has nothing to deliver")
	}
	if posted {
		t.Error("must not attempt to open a new pull request with no title - GitHub would 422 it")
	}
	if len(outcomes) != 1 || outcomes[0].Error == "" {
		t.Fatalf("outcomes = %+v, want a named error", outcomes)
	}
	if strings.Contains(outcomes[0].Error, "422") {
		t.Errorf("outcomes[0].Error = %q, want the named cause, not an opaque 422", outcomes[0].Error)
	}
	if !strings.Contains(outcomes[0].Error, "no open pull request") {
		t.Errorf("outcomes[0].Error = %q, want it to explain no open PR was found", outcomes[0].Error)
	}
}

// Same refusal and no-POST guarantee when findOpenPR itself errors instead of reporting no PR.
func TestDeliverPushWithNoTitleAndFailedLookupFailsExplicitly(t *testing.T) {
	var posted bool
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
			// Malformed body (object, not array): an immediate decode error, no retry.
			io.WriteString(w, `{"not":"an array"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			posted = true
			w.WriteHeader(http.StatusUnprocessableEntity)
			io.WriteString(w, `{"message":"Validation Failed"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	dc := sdk.DeliveryContext{
		GatePassed: true,
		ChatID:     "chat-724-5",
		CloneURL:   "https://github.com/acme/widgets.git",
		Branch:     "feature",
		Items:      []sdk.StagedDelivery{{Kind: "pull_request", TitleOmitted: true, BodyOmitted: true}},
	}
	outcomes, err := app.Deliver(context.Background(), dc)
	if err == nil {
		t.Fatal("Deliver: want an error - the PR lookup failed and there's no title to fall back on")
	}
	if posted {
		t.Error("must not attempt to open a new pull request with no title - GitHub would 422 it")
	}
	if len(outcomes) != 1 || outcomes[0].Error == "" {
		t.Fatalf("outcomes = %+v, want a named error", outcomes)
	}
	if !strings.Contains(outcomes[0].Error, "checking for its open pull request failed") {
		t.Errorf("outcomes[0].Error = %q, want it to name the lookup failure, not a generic delivery error", outcomes[0].Error)
	}
}

// A non-empty dc.PushedSHA isn't proof the branch landed: Deliver confirms the head against GitHub
// (verifyPushedBranch) and fails closed on mismatch. quack-core pushes, so no git binary is needed.
func TestDeliverVerifiesPushAgainstGitHub(t *testing.T) {
	const fullSHA = "abcdef0123456789abcdef0123456789abcdef01"

	dc := sdk.DeliveryContext{
		GatePassed: true, // these test delivery mechanics, not the gate caveat
		CloneURL:   "https://github.com/acme/widgets.git",
		PushedSHA:  fullSHA,
		Branch:     "feature",
		Items:      []sdk.StagedDelivery{{Kind: "pull_request", Title: "Add widget", Body: "adds a widget"}},
	}

	t.Run("mismatched remote head fails closed", func(t *testing.T) {
		dc := dc
		dc.ChatID = "chat-push-fail"
		var prOpened bool
		app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/git/ref/heads/feature"):
				io.WriteString(w, `{"object":{"sha":"0000000000000000000000000000000000000000"}}`)
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
				io.WriteString(w, `[]`)
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
				prOpened = true
				w.WriteHeader(http.StatusCreated)
				io.WriteString(w, `{"html_url":"x","number":1}`)
			default:
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
		})
		_, err := app.Deliver(context.Background(), dc)
		if err == nil {
			t.Fatal("expected an error when the pushed branch isn't reflected on GitHub")
		}
		if !strings.Contains(err.Error(), "not reflected") {
			t.Errorf("error = %v; want it to explain the head mismatch", err)
		}
		if prOpened {
			t.Error("a PR was opened despite failed push verification - must fail closed, never claim delivery")
		}
		d, ok := takeDeliveryDetail("chat-push-fail")
		if !ok || d.err == nil {
			t.Errorf("recorded outcome = %+v, ok=%v; want a recorded failure", d, ok)
		}
	})

	t.Run("verified remote head delivers", func(t *testing.T) {
		dc := dc
		dc.ChatID = "chat-push-ok"
		var prOpened bool
		app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/git/ref/heads/feature"):
				fmt.Fprintf(w, `{"object":{"sha":%q}}`, fullSHA)
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
				io.WriteString(w, `[]`)
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
				prOpened = true
				w.WriteHeader(http.StatusCreated)
				io.WriteString(w, `{"html_url":"https://github.com/acme/widgets/pull/9","number":9}`)
			default:
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
		})
		if _, err := app.Deliver(context.Background(), dc); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if !prOpened {
			t.Error("expected a PR to be opened once the push was verified against GitHub")
		}
		d, ok := takeDeliveryDetail("chat-push-ok")
		if !ok || d.err != nil || d.pushedSHA != fullSHA || d.prNumber != 9 {
			t.Errorf("recorded outcome = %+v, ok=%v; want the verified pushed SHA + pr_number", d, ok)
		}
	})

	// GitHub's git-refs API isn't read-your-writes consistent: retry an early 404 instead of
	// declaring the push a phantom failure.
	t.Run("transient 404 on verification recovers", func(t *testing.T) {
		dc := dc
		dc.ChatID = "chat-push-race"
		var refHits, prOpened int32
		app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/git/ref/heads/feature"):
				if atomic.AddInt32(&refHits, 1) <= 2 {
					http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
					return
				}
				fmt.Fprintf(w, `{"object":{"sha":%q}}`, fullSHA)
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
				io.WriteString(w, `[]`)
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
				atomic.AddInt32(&prOpened, 1)
				w.WriteHeader(http.StatusCreated)
				io.WriteString(w, `{"html_url":"https://github.com/acme/widgets/pull/10","number":10}`)
			default:
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
		})
		if _, err := app.Deliver(context.Background(), dc); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if atomic.LoadInt32(&prOpened) != 1 {
			t.Error("expected the PR to be opened once the racy verification finally landed")
		}
		d, ok := takeDeliveryDetail("chat-push-race")
		if !ok || d.err != nil || d.prNumber != 10 {
			t.Errorf("recorded outcome = %+v, ok=%v; want a successful delivery despite the transient 404s", d, ok)
		}
	})
}

// Re-delivering a staged comment for the same slot edits the prior marked comment, not a duplicate.
func TestDeliverCommentIdempotentEdit(t *testing.T) {
	var posted, patched bool
	var patchedBody string
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/app"):
			io.WriteString(w, `{"slug":"quack"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7"):
			io.WriteString(w, `{"user":{"login":"alice"}}`) // prAuthor: a human, not the bot → normal review path
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			io.WriteString(w, `[{"id":555,"node_id":"NODE555","body":"progress: 40%\n\n<!-- quack:delivery:comment:status -->","user":{"login":"quack[bot]"}}]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			posted = true
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{}`)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/comments/555"):
			patched = true
			data, _ := io.ReadAll(r.Body)
			var b map[string]string
			_ = json.Unmarshal(data, &b)
			patchedBody = b["body"]
			io.WriteString(w, `{}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	dc := sdk.DeliveryContext{
		GatePassed:  true, // these test delivery mechanics, not the gate caveat
		ChatID:      "chat-comment",
		CloneURL:    "https://github.com/acme/widgets.git",
		IssueNumber: 7,
		Items:       []sdk.StagedDelivery{{Kind: "comment", Slot: "status", Body: "progress: 80%"}},
	}
	if _, err := app.Deliver(context.Background(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if posted {
		t.Error("a duplicate comment was POSTed despite a prior quack comment carrying the same slot marker")
	}
	if !patched {
		t.Fatal("the prior comment was not edited in place")
	}
	if !strings.Contains(patchedBody, "progress: 80%") || !strings.Contains(patchedBody, "quack:delivery:comment:status") {
		t.Errorf("patched body = %q; want the new content plus its marker", patchedBody)
	}
}

// A gate-failed comment carries the same caveat banner as a gate-failed PR/review:
// a comment has no draft-equivalent signal to fall back on.
func TestDeliverCommentCarriesGateCaveat(t *testing.T) {
	t.Run("gate failed adds the banner", func(t *testing.T) {
		var postedBody string
		app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/app"):
				io.WriteString(w, `{"slug":"quack"}`)
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
				io.WriteString(w, `[]`)
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
				data, _ := io.ReadAll(r.Body)
				var b map[string]string
				_ = json.Unmarshal(data, &b)
				postedBody = b["body"]
				w.WriteHeader(http.StatusCreated)
				io.WriteString(w, `{}`)
			default:
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
		})

		dc := sdk.DeliveryContext{
			GatePassed:   false,
			GateFeedback: "answer was scaffolding, not an implementation",
			ChatID:       "chat-comment-unvetted",
			CloneURL:     "https://github.com/acme/widgets.git",
			IssueNumber:  7,
			Items:        []sdk.StagedDelivery{{Kind: "comment", Slot: "status", Body: "<env>...</env>"}},
		}
		if _, err := app.Deliver(context.Background(), dc); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if !strings.Contains(postedBody, "did not pass") {
			t.Fatalf("gate-failed comment missing the caveat banner: %q", postedBody)
		}
	})

	t.Run("gate passed has no banner", func(t *testing.T) {
		var postedBody string
		app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case strings.HasSuffix(r.URL.Path, "/app"):
				io.WriteString(w, `{"slug":"quack"}`)
			case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
				io.WriteString(w, `[]`)
			case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
				data, _ := io.ReadAll(r.Body)
				var b map[string]string
				_ = json.Unmarshal(data, &b)
				postedBody = b["body"]
				w.WriteHeader(http.StatusCreated)
				io.WriteString(w, `{}`)
			default:
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			}
		})

		dc := sdk.DeliveryContext{
			GatePassed:  true,
			ChatID:      "chat-comment-vetted",
			CloneURL:    "https://github.com/acme/widgets.git",
			IssueNumber: 7,
			Items:       []sdk.StagedDelivery{{Kind: "comment", Slot: "status", Body: "progress: 80%"}},
		}
		if _, err := app.Deliver(context.Background(), dc); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if strings.Contains(postedBody, "did not pass") {
			t.Fatalf("gate-passed comment must not carry the caveat banner: %q", postedBody)
		}
	})
}

// Before submitting a new review, Deliver minimizes (GraphQL minimizeComment) any prior marked quack review.
func TestDeliverCollapsesPriorReview(t *testing.T) {
	var minimizedID string
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/app"):
			io.WriteString(w, `{"slug":"quack"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7"):
			io.WriteString(w, `{"user":{"login":"alice"}}`) // prAuthor: a human, not the bot → normal review path
		case strings.HasSuffix(r.URL.Path, "/pulls/7/files"):
			io.WriteString(w, `[]`) // no inline comments drafted
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reviews"):
			io.WriteString(w, `[{"node_id":"REVIEW1","body":"old findings\n\n<!-- quack:delivery:review -->","state":"COMMENTED","user":{"login":"quack[bot]"}}]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reviews"):
			io.WriteString(w, `{"id":2,"html_url":"https://github.com/acme/widgets/pull/7#pullrequestreview-2"}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/graphql"):
			data, _ := io.ReadAll(r.Body)
			var body struct {
				Variables struct {
					ID string `json:"id"`
				} `json:"variables"`
			}
			_ = json.Unmarshal(data, &body)
			minimizedID = body.Variables.ID
			io.WriteString(w, `{"data":{"minimizeComment":{"minimizedComment":{"isMinimized":true}}}}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	dc := sdk.DeliveryContext{
		GatePassed:  true, // these test delivery mechanics, not the gate caveat
		ChatID:      "chat-review",
		CloneURL:    "https://github.com/acme/widgets.git",
		IssueNumber: 7,
		Items:       []sdk.StagedDelivery{{Kind: "review", Event: "comment", Body: "new findings"}},
	}
	if _, err := app.Deliver(context.Background(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if minimizedID != "REVIEW1" {
		t.Errorf("minimizeComment subjectId = %q; want the prior review's node_id REVIEW1", minimizedID)
	}
}

// On a quack-authored PR GitHub 422s approve/request_changes, but a COMMENT-event review is allowed
// and must still carry findings as real inline comments[], not flattened text.
func TestDeliverReviewOnOwnPRIsCommentNoVerdict(t *testing.T) {
	var reviewBody []byte
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/app"):
			io.WriteString(w, `{"slug":"quack"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7"):
			io.WriteString(w, `{"user":{"login":"quack[bot]"}}`) // quack authored this PR
		case strings.HasSuffix(r.URL.Path, "/pulls/7/files"):
			io.WriteString(w, `[{"filename":"main.go","patch":"@@ -42,1 +42,1 @@\n-old\n+new"}]`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reviews"):
			io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls/7/reviews"):
			reviewBody, _ = io.ReadAll(r.Body)
			io.WriteString(w, `{"id":9,"html_url":"https://github.com/acme/widgets/pull/7#pullrequestreview-9"}`)
		case strings.HasSuffix(r.URL.Path, "/issues/7/comments"):
			t.Error("must deliver the own-PR review as a review, not a flattened issue comment")
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	dc := sdk.DeliveryContext{
		GatePassed: true, ChatID: "chat-ownpr", CloneURL: "https://github.com/acme/widgets.git", IssueNumber: 7,
		Items: []sdk.StagedDelivery{{Kind: "review", Event: "approve", Body: "clean change",
			Comments: []sdk.ReviewComment{{Path: "main.go", Line: 42, Body: "tiny nit"}}}},
	}
	if _, err := app.Deliver(context.Background(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	var posted struct {
		Event    string `json:"event"`
		Body     string `json:"body"`
		Comments []struct {
			Path string `json:"path"`
			Line int    `json:"line"`
			Body string `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(reviewBody, &posted); err != nil {
		t.Fatal(err)
	}
	if posted.Event != "COMMENT" {
		t.Fatalf("review event = %q; want COMMENT (own PR can't carry approve/request_changes)", posted.Event)
	}
	if !strings.Contains(posted.Body, "clean change") {
		t.Fatalf("self-review body missing the summary:\n%s", posted.Body)
	}
	if !strings.Contains(posted.Body, "<!-- quack:delivery:review:approve -->") {
		t.Fatalf("self-review body missing its verdict marker (needed by the quack:merge gate, #482):\n%s", posted.Body)
	}
	if len(posted.Comments) != 1 || posted.Comments[0].Path != "main.go" || posted.Comments[0].Line != 42 {
		t.Fatalf("finding did not land as an inline review comment (#513): %s", reviewBody)
	}
}

// An own-PR review comment has no commit_id, so quack embeds the reviewed head SHA in the body
// for merge.go's staleness guard.
func TestDeliverReviewOnOwnPREmbedsHeadMarker(t *testing.T) {
	var reviewBody []byte
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/app"):
			io.WriteString(w, `{"slug":"quack"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7"):
			io.WriteString(w, `{"user":{"login":"quack[bot]"},"head":{"ref":"feature","sha":"head1"},"base":{"ref":"main"}}`)
		case strings.HasSuffix(r.URL.Path, "/pulls/7/files"):
			io.WriteString(w, `[]`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reviews"):
			io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls/7/reviews"):
			reviewBody, _ = io.ReadAll(r.Body)
			io.WriteString(w, `{"id":9,"html_url":"https://github.com/acme/widgets/pull/7#pullrequestreview-9"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	dc := sdk.DeliveryContext{
		GatePassed: true, ChatID: "chat-ownpr-head", CloneURL: "https://github.com/acme/widgets.git", IssueNumber: 7,
		Items: []sdk.StagedDelivery{{Kind: "review", Event: "approve", Body: "clean change"}},
	}
	if _, err := app.Deliver(context.Background(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	var posted struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(reviewBody, &posted); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(posted.Body, "<!-- quack:delivery:head:head1 -->") {
		t.Fatalf("self-review body missing its head marker (needed to pin the merge staleness guard):\n%s", posted.Body)
	}
}

// The reviewer's machine-parseable VERDICT/FINDINGS tail and any fallback preamble don't belong
// in the human-facing own-PR review body.
func TestDeliverReviewOnOwnPRStripsVerdictTail(t *testing.T) {
	var reviewBody []byte
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/app"):
			io.WriteString(w, `{"slug":"quack"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7"):
			io.WriteString(w, `{"user":{"login":"quack[bot]"}}`)
		case strings.HasSuffix(r.URL.Path, "/pulls/7/files"):
			io.WriteString(w, `[{"filename":"main.go","patch":"@@ -42,1 +42,1 @@\n-old\n+new"}]`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reviews"):
			io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls/7/reviews"):
			reviewBody, _ = io.ReadAll(r.Body)
			io.WriteString(w, `{"id":9,"html_url":"https://github.com/acme/widgets/pull/7#pullrequestreview-9"}`)
		case strings.HasSuffix(r.URL.Path, "/issues/7/comments"):
			t.Error("must deliver the own-PR review as a review, not a flattened issue comment")
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	rawAnswer := "Since staging tools aren't available in this environment, here is the full structured review as the fallback output format:\n\n" +
		"This change looks solid overall.\n\n" +
		"VERDICT: approve\n" +
		"FINDINGS:\n" +
		"- main.go:42: tiny nit\n"
	dc := sdk.DeliveryContext{
		GatePassed: true, ChatID: "chat-ownpr-tail", CloneURL: "https://github.com/acme/widgets.git", IssueNumber: 7,
		Items: []sdk.StagedDelivery{{Kind: "review", Event: "approve", Body: rawAnswer,
			Comments: []sdk.ReviewComment{{Path: "main.go", Line: 42, Body: "tiny nit"}}}},
	}
	if _, err := app.Deliver(context.Background(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	var posted struct {
		Body     string `json:"body"`
		Comments []struct {
			Path string `json:"path"`
			Line int    `json:"line"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(reviewBody, &posted); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(posted.Body, "This change looks solid overall.") {
		t.Fatalf("self-review body dropped the human-facing summary:\n%s", posted.Body)
	}
	if strings.Contains(posted.Body, "VERDICT:") || strings.Contains(posted.Body, "FINDINGS:") {
		t.Fatalf("self-review body leaked the machine-parseable tail:\n%s", posted.Body)
	}
	if strings.Contains(posted.Body, "fallback output format") {
		t.Fatalf("self-review body leaked the fallback-format preamble:\n%s", posted.Body)
	}
	if !strings.Contains(posted.Body, "<!-- quack:delivery:review:approve -->") {
		t.Fatalf("self-review body missing its verdict marker:\n%s", posted.Body)
	}
	if len(posted.Comments) != 1 || posted.Comments[0].Path != "main.go" || posted.Comments[0].Line != 42 {
		t.Fatalf("finding did not land as an inline review comment (#513): %s", reviewBody)
	}
}

// An external (ACP) reviewer's staged review carries gate-parsed inline comments and no ledger PR number;
// delivery posts the comments and recovers the PR from the chat id.
func TestDeliverReviewInlineCommentsAndChatIDPR(t *testing.T) {
	var reviewBody []byte
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/app"):
			io.WriteString(w, `{"slug":"quack"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7"):
			io.WriteString(w, `{"user":{"login":"alice"}}`) // prAuthor: a human, not the bot → normal review path
		case strings.HasSuffix(r.URL.Path, "/pulls/7/files"):
			// main.go line 42 commentable on the RIGHT side.
			io.WriteString(w, `[{"filename":"main.go","patch":"@@ -42,1 +42,1 @@\n-old\n+new"}]`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reviews"):
			io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls/7/reviews"):
			reviewBody, _ = io.ReadAll(r.Body)
			io.WriteString(w, `{"id":9,"html_url":"https://github.com/acme/widgets/pull/7#pullrequestreview-9"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	dc := sdk.DeliveryContext{
		GatePassed: true,
		ChatID:     "github-acme-widgets-7", // the webhook dispatch session id - the PR number source
		CloneURL:   "https://github.com/acme/widgets.git",
		Items: []sdk.StagedDelivery{{
			Kind: "review", Event: "request_changes", Body: "two blockers",
			Comments: []sdk.ReviewComment{{Path: "main.go", Line: 42, Body: "route shadowed"}},
		}},
	}
	if _, err := app.Deliver(context.Background(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	var posted struct {
		Comments []struct {
			Path string `json:"path"`
			Line int    `json:"line"`
			Body string `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(reviewBody, &posted); err != nil {
		t.Fatal(err)
	}
	if len(posted.Comments) != 1 || posted.Comments[0].Path != "main.go" || posted.Comments[0].Line != 42 {
		t.Fatalf("inline comments not posted: %s", reviewBody)
	}
}

// A finding on a line outside the diff (GitHub refuses inline comments there) lands as an inline comment
// on the nearest commentable line, so a later fix run finds it like any anchored finding.
func TestDeliverReviewReanchorsUncommentableFinding(t *testing.T) {
	var reviewBody []byte
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/app"):
			io.WriteString(w, `{"slug":"quack"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7"):
			io.WriteString(w, `{"user":{"login":"alice"}}`)
		case strings.HasSuffix(r.URL.Path, "/pulls/7/files"):
			// ManageDBActivity.kt: lines 12-20 changed; line 50 is context, outside the diff.
			io.WriteString(w, `[{"filename":"manageDB/ManageDBActivity.kt","patch":"@@ -12,4 +12,9 @@\n context\n+added1\n+added2\n+added3\n+added4\n+added5\n+added6\n+added7\n context"}]`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reviews"):
			io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls/7/reviews"):
			reviewBody, _ = io.ReadAll(r.Body)
			io.WriteString(w, `{"id":9,"html_url":"https://github.com/acme/widgets/pull/7#pullrequestreview-9"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	dc := sdk.DeliveryContext{
		GatePassed: true, ChatID: "chat-reanchor", CloneURL: "https://github.com/acme/widgets.git", IssueNumber: 7,
		Items: []sdk.StagedDelivery{{
			Kind: "review", Event: "request_changes", Body: "one blocker",
			Comments: []sdk.ReviewComment{{
				Path: "manageDB/ManageDBActivity.kt", Line: 50,
				Body: "double ViewModel instantiation causes deletions not to reflect in the list",
			}},
		}},
	}
	if _, err := app.Deliver(context.Background(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	var posted struct {
		Body     string `json:"body"`
		Comments []struct {
			Path string `json:"path"`
			Line int    `json:"line"`
			Body string `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(reviewBody, &posted); err != nil {
		t.Fatal(err)
	}
	if len(posted.Comments) != 1 {
		t.Fatalf("the blocking finding must survive as an inline comment, re-anchored: %s", reviewBody)
	}
	c := posted.Comments[0]
	if c.Path != "manageDB/ManageDBActivity.kt" {
		t.Errorf("comment path = %q", c.Path)
	}
	if c.Line == 50 {
		t.Errorf("line 50 is not commentable - GitHub would 422 this")
	}
	if !strings.Contains(c.Body, "line 50") {
		t.Errorf("re-anchored comment doesn't state its true location: %q", c.Body)
	}
	if !strings.Contains(c.Body, "double ViewModel instantiation") {
		t.Errorf("re-anchored comment lost the finding text: %q", c.Body)
	}
}

// A finding on a pure-deletion hunk can't be re-anchored, so it must reach the review body
// as a distinguishable, located item, not dropped or folded into prose.
func TestDeliverReviewKeepsUnanchorableFindingInBody(t *testing.T) {
	var reviewBody []byte
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/app"):
			io.WriteString(w, `{"slug":"quack"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7"):
			io.WriteString(w, `{"user":{"login":"alice"}}`)
		case strings.HasSuffix(r.URL.Path, "/pulls/7/files"):
			io.WriteString(w, `[{"filename":"dead.go","patch":"@@ -10,3 +10,0 @@\n-a\n-b\n-c"}]`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reviews"):
			io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls/7/reviews"):
			reviewBody, _ = io.ReadAll(r.Body)
			io.WriteString(w, `{"id":9,"html_url":"https://github.com/acme/widgets/pull/7#pullrequestreview-9"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	dc := sdk.DeliveryContext{
		GatePassed: true, ChatID: "chat-unanchorable", CloneURL: "https://github.com/acme/widgets.git", IssueNumber: 7,
		Items: []sdk.StagedDelivery{{
			Kind: "review", Event: "request_changes", Body: "one blocker",
			Comments: []sdk.ReviewComment{{Path: "dead.go", Line: 15, Body: "removed code still referenced elsewhere"}},
		}},
	}
	if _, err := app.Deliver(context.Background(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	var posted struct {
		Body     string     `json:"body"`
		Comments []struct{} `json:"comments"`
	}
	if err := json.Unmarshal(reviewBody, &posted); err != nil {
		t.Fatal(err)
	}
	if len(posted.Comments) != 0 {
		t.Fatalf("dead.go has no commentable line - nothing should post inline: %s", reviewBody)
	}
	if !strings.Contains(posted.Body, "one blocker") {
		t.Fatalf("review body lost its own summary: %q", posted.Body)
	}
	if !strings.Contains(posted.Body, "dead.go") || !strings.Contains(posted.Body, "line 15") {
		t.Fatalf("review body doesn't identify the unanchored finding's true location: %q", posted.Body)
	}
	if !strings.Contains(posted.Body, "removed code still referenced elsewhere") {
		t.Fatalf("review body lost the unanchored finding text: %q", posted.Body)
	}
}

// A review-only delivery never pushes: dc.PushedSHA is unset, so the git/ref endpoint must never be hit.
func TestDeliverReviewNeverPushesBranch(t *testing.T) {
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/app"):
			io.WriteString(w, `{"slug":"quack"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7"):
			io.WriteString(w, `{"user":{"login":"alice"}}`) // prAuthor: a human, not the bot → normal review path
		case strings.HasSuffix(r.URL.Path, "/pulls/7/files"):
			io.WriteString(w, `[]`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reviews"):
			io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls/7/reviews"):
			io.WriteString(w, `{"id":9,"html_url":"https://github.com/acme/widgets/pull/7#pullrequestreview-9"}`)
		case strings.Contains(r.URL.Path, "/git/ref/"):
			// The push-verify endpoint - reaching it means a push was attempted.
			t.Errorf("review delivery must never push/verify a branch, got %s %s", r.Method, r.URL.Path)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
	dc := sdk.DeliveryContext{
		GatePassed: true,
		ChatID:     "github-acme-widgets-7",
		CloneURL:   "https://github.com/acme/widgets.git",
		// PushedSHA intentionally empty - a review-only item never carries one.
		Branch: "some-pr-branch",
		Items:  []sdk.StagedDelivery{{Kind: "review", Event: "approve", Body: "looks good"}},
	}
	if _, err := app.Deliver(context.Background(), dc); err != nil {
		t.Fatalf("review-only Deliver should succeed without any push: %v", err)
	}
}

// A review always posts: failing checks are the merge's problem (branch protection), not the review's.
func TestDeliverApprovePostsDespiteFailingChecks(t *testing.T) {
	var posted bool
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/app"):
			io.WriteString(w, `{"slug":"quack"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7"):
			io.WriteString(w, `{"user":{"login":"alice"},"head":{"sha":"deadbeef"}}`)
		case strings.HasSuffix(r.URL.Path, "/pulls/7/files"):
			io.WriteString(w, `[]`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reviews"):
			io.WriteString(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/pulls/7/reviews"):
			posted = true
			io.WriteString(w, `{"id":9,"html_url":"https://github.com/acme/widgets/pull/7#pullrequestreview-9"}`)
		case strings.Contains(r.URL.Path, "/check-runs"):
			t.Error("delivery must not re-check CI - reviews post regardless of check state")
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	dc := sdk.DeliveryContext{
		GatePassed: true,
		ChatID:     "github-acme-widgets-7",
		CloneURL:   "https://github.com/acme/widgets.git",
		Items:      []sdk.StagedDelivery{{Kind: "review", Event: "approve", Body: "looks good"}},
	}
	if _, err := app.Deliver(context.Background(), dc); err != nil {
		t.Fatalf("Deliver should post the approve regardless of CI state: %v", err)
	}
	if !posted {
		t.Error("approve should have reached GitHub")
	}
}

// A gate FAIL still delivers the PR (a human decides) but opens it as a DRAFT
// so it cannot be merged accidentally.
func TestDeliverFailedGateOpensDraftPR(t *testing.T) {
	var prBody []byte
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/app"):
			io.WriteString(w, `{"slug":"quack"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7"):
			io.WriteString(w, `{"user":{"login":"alice"}}`) // prAuthor: a human, not the bot → normal review path
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
			io.WriteString(w, `[]`) // no existing open PR
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/3"):
			io.WriteString(w, `{"title":"t","body":"b","state":"open","labels":[]}`) // withClosesTrailer's partial-fix check
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			prBody, _ = io.ReadAll(r.Body)
			io.WriteString(w, `{"html_url":"https://github.com/acme/widgets/pull/8","number":8}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	dc := sdk.DeliveryContext{
		GatePassed:   false,
		GateFeedback: "tests fail",
		ChatID:       "github-acme-widgets-3",
		CloneURL:     "https://github.com/acme/widgets.git",
		Branch:       "quack/fix",
		Items:        []sdk.StagedDelivery{{Kind: "pull_request", Title: "Fix it", Body: "the fix"}},
	}
	if _, err := app.Deliver(context.Background(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	var posted struct {
		Draft bool   `json:"draft"`
		Body  string `json:"body"`
	}
	if err := json.Unmarshal(prBody, &posted); err != nil {
		t.Fatal(err)
	}
	if !posted.Draft {
		t.Fatalf("gate-failed PR must open as a draft: %s", prBody)
	}
	if !strings.Contains(posted.Body, "did not pass") {
		t.Fatalf("caveat banner missing from body: %s", posted.Body)
	}
	// A fresh PR opened for a chat tied to issue #3 closes it deterministically.
	if !strings.Contains(posted.Body, "Closes #3") {
		t.Fatalf("delivered PR body missing deterministic Closes trailer: %s", posted.Body)
	}
}

// The quack:partial-fix label on the originating issue suppresses the Closes trailer:
// the maintainer's explicit "this PR does not close it" signal.
func TestDeliverSuppressesClosesTrailerWhenPartialFix(t *testing.T) {
	var prBody []byte
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
			io.WriteString(w, `[]`) // no existing open PR
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/9"):
			io.WriteString(w, `{"title":"t","body":"b","state":"open","labels":[{"name":"quack:partial-fix"}]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			prBody, _ = io.ReadAll(r.Body)
			io.WriteString(w, `{"html_url":"https://github.com/acme/widgets/pull/10","number":10}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
	dc := sdk.DeliveryContext{
		GatePassed: true,
		ChatID:     "github-acme-widgets-9",
		CloneURL:   "https://github.com/acme/widgets.git",
		Branch:     "quack/partial",
		Items:      []sdk.StagedDelivery{{Kind: "pull_request", Title: "Partial fix", Body: "does part of it"}},
	}
	if _, err := app.Deliver(context.Background(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	var posted struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(prBody, &posted); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(posted.Body, "Closes #9") {
		t.Fatalf("partial-fix issue must not get an unconditional Closes trailer: %s", posted.Body)
	}
}

// A PR-scoped chat id whose original PR was closed also takes the fresh-open path, but its number is a PR,
// not an issue; GitHub's issues endpoint returns PRs too, so it must not get a Closes trailer.
func TestDeliverSkipsClosesTrailerWhenChatIDResolvesToAPR(t *testing.T) {
	var prBody []byte
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
			io.WriteString(w, `[]`) // no OPEN PR on this branch - the original PR #92 was closed/merged
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/92"):
			// GitHub marks an issues-endpoint response as actually being a PR via
			// the pull_request field.
			io.WriteString(w, `{"title":"t","body":"b","state":"closed","pull_request":{},"labels":[]}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			prBody, _ = io.ReadAll(r.Body)
			io.WriteString(w, `{"html_url":"https://github.com/acme/widgets/pull/93","number":93}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
	dc := sdk.DeliveryContext{
		GatePassed: true,
		ChatID:     "github-acme-widgets-92",
		CloneURL:   "https://github.com/acme/widgets.git",
		Branch:     "quack/fix-92",
		Items:      []sdk.StagedDelivery{{Kind: "pull_request", Title: "Redo the fix", Body: "the fix"}},
	}
	if _, err := app.Deliver(context.Background(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	var posted struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(prBody, &posted); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(posted.Body, "Closes") {
		t.Fatalf("chat id resolving to a PULL REQUEST must never get a Closes trailer: %s", posted.Body)
	}
}

// A PR update (an already-open PR on the branch) never gets a Closes trailer: the chat id's number
// is the PR's own, not an issue to close.
func TestDeliverDoesNotAppendClosesOnPRUpdate(t *testing.T) {
	var patchedBody map[string]string
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
			io.WriteString(w, `[{"number":11,"html_url":"https://github.com/acme/widgets/pull/11"}]`)
		case r.Method == http.MethodPatch && strings.HasSuffix(r.URL.Path, "/pulls/11"):
			data, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(data, &patchedBody)
			io.WriteString(w, `{"html_url":"https://github.com/acme/widgets/pull/11"}`)
		case strings.HasSuffix(r.URL.Path, "/issues/11"):
			t.Error("an update to an existing PR must never look up the partial-fix label")
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
	dc := sdk.DeliveryContext{
		GatePassed: true,
		ChatID:     "github-acme-widgets-11", // same number as the PR itself
		CloneURL:   "https://github.com/acme/widgets.git",
		Branch:     "fix/11-something",
		Items:      []sdk.StagedDelivery{{Kind: "pull_request", Title: "Fix it", Body: "the fix"}},
	}
	if _, err := app.Deliver(context.Background(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if strings.Contains(patchedBody["body"], "Closes") {
		t.Fatalf("PR update must never gain a Closes trailer: %+v", patchedBody)
	}
}

// A body that already references the issue with a closing keyword gets no second trailer
// and needs no partial-fix lookup.
func TestDeliverClosesTrailerNotDuplicated(t *testing.T) {
	var prBody []byte
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
			io.WriteString(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/issues/5"):
			t.Error("a body that already closes the issue must not trigger a partial-fix lookup")
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			prBody, _ = io.ReadAll(r.Body)
			io.WriteString(w, `{"html_url":"https://github.com/acme/widgets/pull/6","number":6}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})
	dc := sdk.DeliveryContext{
		GatePassed: true,
		ChatID:     "github-acme-widgets-5",
		CloneURL:   "https://github.com/acme/widgets.git",
		Branch:     "quack/fix5",
		Items:      []sdk.StagedDelivery{{Kind: "pull_request", Title: "Fix it", Body: "does the work.\n\nCloses #5\n"}},
	}
	if _, err := app.Deliver(context.Background(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	var posted struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(prBody, &posted); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(posted.Body, "Closes #5"); n != 1 {
		t.Fatalf("Closes #5 appears %d times, want exactly 1: %s", n, posted.Body)
	}
}

// A review delivered with dc.IdempotencyKey embeds the key marker via the real Deliver path:
// what RecoverDelivery later searches for.
func TestDeliverReviewEmbedsIdempotencyKeyMarker(t *testing.T) {
	var reviewBody []byte
	app := newDeliveryApp(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/app"):
			io.WriteString(w, `{"slug":"quack"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7"):
			io.WriteString(w, `{"user":{"login":"alice"}}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/reviews"):
			io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls/7/reviews"):
			reviewBody, _ = io.ReadAll(r.Body)
			io.WriteString(w, `{"id":9,"html_url":"https://github.com/acme/widgets/pull/7#pullrequestreview-9"}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	})

	dc := sdk.DeliveryContext{
		GatePassed:     true,
		ChatID:         "github-acme-widgets-7",
		CloneURL:       "https://github.com/acme/widgets.git",
		IssueNumber:    7,
		IdempotencyKey: "code_review:pr:7@3",
		Items:          []sdk.StagedDelivery{{Kind: "review", Event: "approve", Body: "looks good"}},
	}
	if _, err := app.Deliver(context.Background(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	var posted struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal(reviewBody, &posted); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(posted.Body, deliveryKeyMarker("code_review:pr:7@3")) {
		t.Fatalf("posted body = %q, want the idempotency key marker embedded", posted.Body)
	}
}
