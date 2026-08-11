package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fagerbergj/quack/internal/vetting"
)

// newDeliveryApp returns an App wired to an httptest server for App.Deliver
// tests, its install/token caches pre-seeded so only the stubbed endpoints
// are hit (mirrors newReviewApp/seededApp in tools_test.go).
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

// TestDeliverPullRequestUpdatesExistingInsteadOfDuplicate pins a staged
// pull_request delivered against a branch that already has an OPEN PR must
// UPDATE that PR, never open a second one.
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

	dc := vetting.DeliveryContext{
		GatePassed: true, // these test delivery mechanics, not the gate caveat
		ChatID:     "chat-1",
		CloneURL:   "https://github.com/acme/widgets.git",
		Branch:     "feature", // PushedSHA empty ⇒ Deliver skips verify, goes straight to the items
		Items:      []vetting.StagedDelivery{{Kind: "pull_request", Title: "Add widget", Body: "does the thing"}},
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

// TestDeliverPushWithNothingToSayOmitsThePatch pins #724 test case 1: a
// stage_push with no title/body must leave the existing PR completely
// untouched - openOrUpdatePullRequest reuses findOpenPR's own url/number
// instead of round-tripping a no-op PATCH, so no PATCH request happens at all.
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

	dc := vetting.DeliveryContext{
		GatePassed: true,
		ChatID:     "chat-724-1",
		CloneURL:   "https://github.com/acme/widgets.git",
		Branch:     "feature",
		Items:      []vetting.StagedDelivery{{Kind: "pull_request", TitleOmitted: true, BodyOmitted: true}},
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

// TestDeliverPushWithDeliberateUpdateAppliesBoth pins #724 test case 2:
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

	dc := vetting.DeliveryContext{
		GatePassed: true,
		ChatID:     "chat-724-2",
		CloneURL:   "https://github.com/acme/widgets.git",
		Branch:     "feature",
		Items:      []vetting.StagedDelivery{{Kind: "pull_request", Title: "fix: retry flaky upload", Body: "adds a backoff"}},
	}
	if _, err := app.Deliver(context.Background(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if len(patchedBody) != 2 || patchedBody["title"] != "fix: retry flaky upload" || patchedBody["body"] != "adds a backoff" {
		t.Errorf("PATCH body = %+v, want exactly title and body set", patchedBody)
	}
}

// TestDeliverPushPartialUpdateOmitsTitleKey pins #724 test case 3: a
// body-only stage_push must PATCH with no "title" key at all, not an empty
// string - an empty string would blank the PR's real title on GitHub.
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

	dc := vetting.DeliveryContext{
		GatePassed: true,
		ChatID:     "chat-724-3",
		CloneURL:   "https://github.com/acme/widgets.git",
		Branch:     "feature",
		Items:      []vetting.StagedDelivery{{Kind: "pull_request", Body: "adds a backoff and jitter", TitleOmitted: true}},
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

// TestDeliverPushWithNoTitleAndNoExistingPRFailsExplicitly closes a gap
// openOrUpdatePullRequest had: a titleless stage_push falls through to
// openPullRequest exactly like a legitimate new-PR run whenever no open PR
// is found for the branch (e.g. it was closed between trigger and delivery)
// - but a push run has no title to open one with. GitHub would 422 a
// titleless PR; inventing a title is the exact fabrication #724 removed
// stage_pr's compulsion to do. Delivery must refuse with a named cause
// instead, and never call the create-PR endpoint.
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

	dc := vetting.DeliveryContext{
		GatePassed: true,
		ChatID:     "chat-724-4",
		CloneURL:   "https://github.com/acme/widgets.git",
		Branch:     "feature",
		Items:      []vetting.StagedDelivery{{Kind: "pull_request", TitleOmitted: true, BodyOmitted: true}},
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

// TestDeliverPushWithNoTitleAndFailedLookupFailsExplicitly is the sibling
// case: findOpenPR itself errors (a transient API blip) rather than cleanly
// reporting no PR - same refusal, same no-POST guarantee.
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

	dc := vetting.DeliveryContext{
		GatePassed: true,
		ChatID:     "chat-724-5",
		CloneURL:   "https://github.com/acme/widgets.git",
		Branch:     "feature",
		Items:      []vetting.StagedDelivery{{Kind: "pull_request", TitleOmitted: true, BodyOmitted: true}},
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

// requireGitBinary skips a test when git isn't on PATH.
func requireGitBinary(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not found on PATH")
	}
}

// runGitTest runs one git command in dir, failing the test on error.
func runGitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestDeliverVerifiesPushAgainstGitHub pins that a gate-owned push
// (dc.PushedSHA, set by vetting.commitDelivery before Deliver ever runs) is
// not itself proof the branch landed - Deliver must confirm the branch's
// head against GitHub's OWN state before opening/updating anything, and
// fail closed (no PR, no summary claiming success) when it doesn't match.
func TestDeliverVerifiesPushAgainstGitHub(t *testing.T) {
	requireGitBinary(t)

	// A real commit purely to mint a realistic SHA - no push happens here;
	// that's the gate's job now, already done by the time Deliver runs.
	clone := t.TempDir()
	runGitTest(t, clone, "init", "-q", "--initial-branch=feature")
	if err := os.WriteFile(filepath.Join(clone, "widget.go"), []byte("package widget\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, clone, "add", "-A")
	runGitTest(t, clone, "-c", "user.name=test", "-c", "user.email=test@x.local", "commit", "--quiet", "-m", "add widget")
	fullSHA := runGitTest(t, clone, "rev-parse", "feature")

	dc := vetting.DeliveryContext{
		GatePassed: true, // these test delivery mechanics, not the gate caveat
		CloneURL:   "https://github.com/acme/widgets.git",
		Branch:     "feature",
		PushedSHA:  fullSHA,
		Items:      []vetting.StagedDelivery{{Kind: "pull_request", Title: "Add widget", Body: "adds a widget"}},
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

	// #570: GitHub's git-refs API isn't read-your-writes consistent - a ref
	// lookup right after an accepted push can 404 before it settles. Delivery
	// must retry that instead of declaring the push a phantom failure.
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

// TestDeliverCommentIdempotentEdit pins re-delivering a staged comment
// for the SAME slot must EDIT the prior quack-authored comment carrying its
// marker, not pile up a duplicate.
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

	dc := vetting.DeliveryContext{
		GatePassed:  true, // these test delivery mechanics, not the gate caveat
		ChatID:      "chat-comment",
		CloneURL:    "https://github.com/acme/widgets.git",
		IssueNumber: 7,
		Items:       []vetting.StagedDelivery{{Kind: "comment", Slot: "status", Body: "progress: 80%"}},
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

// TestDeliverCommentCarriesGateCaveat pins #709: a gate-failed comment must
// carry the same caveat banner as a gate-failed PR/review, since a comment
// has no draft-equivalent signal to fall back on.
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

		dc := vetting.DeliveryContext{
			GatePassed:   false,
			GateFeedback: "answer was scaffolding, not an implementation",
			ChatID:       "chat-comment-unvetted",
			CloneURL:     "https://github.com/acme/widgets.git",
			IssueNumber:  7,
			Items:        []vetting.StagedDelivery{{Kind: "comment", Slot: "status", Body: "<env>...</env>"}},
		}
		if _, err := app.Deliver(context.Background(), dc); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if !strings.Contains(postedBody, "did NOT pass") {
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

		dc := vetting.DeliveryContext{
			GatePassed:  true,
			ChatID:      "chat-comment-vetted",
			CloneURL:    "https://github.com/acme/widgets.git",
			IssueNumber: 7,
			Items:       []vetting.StagedDelivery{{Kind: "comment", Slot: "status", Body: "progress: 80%"}},
		}
		if _, err := app.Deliver(context.Background(), dc); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
		if strings.Contains(postedBody, "did NOT pass") {
			t.Fatalf("gate-passed comment must not carry the caveat banner: %q", postedBody)
		}
	})
}

// TestDeliverCollapsesPriorReview pins review half: before submitting a
// new review, Deliver minimizes (GraphQL minimizeComment) any prior
// quack-authored review carrying the review marker.
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

	dc := vetting.DeliveryContext{
		GatePassed:  true, // these test delivery mechanics, not the gate caveat
		ChatID:      "chat-review",
		CloneURL:    "https://github.com/acme/widgets.git",
		IssueNumber: 7,
		Items:       []vetting.StagedDelivery{{Kind: "review", Event: "comment", Body: "new findings"}},
	}
	if _, err := app.Deliver(context.Background(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if minimizedID != "REVIEW1" {
		t.Errorf("minimizeComment subjectId = %q; want the prior review's node_id REVIEW1", minimizedID)
	}
}

// A review on a PR quack authored can't carry an approve/request_changes
// verdict (GitHub 422s an author approving their own PR) - but a COMMENT-event
// review IS allowed, and #513 pins that it must still carry the findings as
// real inline comments[], not flattened text.
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

	dc := vetting.DeliveryContext{
		GatePassed: true, ChatID: "chat-ownpr", CloneURL: "https://github.com/acme/widgets.git", IssueNumber: 7,
		Items: []vetting.StagedDelivery{{Kind: "review", Event: "approve", Body: "clean change",
			Comments: []vetting.ReviewComment{{Path: "main.go", Line: 42, Body: "tiny nit"}}}},
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

// TestDeliverReviewOnOwnPRStripsVerdictTail pins #482: the raw ACP reviewer
// answer carries a machine-parseable VERDICT/FINDINGS tail (for
// augmentFromAnswer) and sometimes a fallback-format preamble - neither
// belongs in the human-facing own-PR review body.
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
	dc := vetting.DeliveryContext{
		GatePassed: true, ChatID: "chat-ownpr-tail", CloneURL: "https://github.com/acme/widgets.git", IssueNumber: 7,
		Items: []vetting.StagedDelivery{{Kind: "review", Event: "approve", Body: rawAnswer,
			Comments: []vetting.ReviewComment{{Path: "main.go", Line: 42, Body: "tiny nit"}}}},
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

// An external (ACP) reviewer's staged review carries gate-parsed inline
// comments and no ledger PR number - delivery posts the comments and recovers
// the PR from the GitHub-dispatched chat id.
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

	dc := vetting.DeliveryContext{
		GatePassed: true,
		ChatID:     "github-acme-widgets-7", // the webhook dispatch session id - the PR number source
		CloneURL:   "https://github.com/acme/widgets.git",
		Items: []vetting.StagedDelivery{{
			Kind: "review", Event: "request_changes", Body: "two blockers",
			Comments: []vetting.ReviewComment{{Path: "main.go", Line: 42, Body: "route shadowed"}},
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

// TestDeliverReviewReanchorsUncommentableFinding reproduces #694 end to end:
// NightsOut#97's judge-flagged BLOCKING finding cited ManageDBActivity.kt:50,
// a line outside the diff (context, not changed) that GitHub refuses inline
// comments on. It must land as a real inline comment on the nearest
// commentable line - the same discrete, GET /pulls/{n}/comments-visible form
// as every anchored finding - not survive only as a body sentence, so a later
// fix run finds it at the same rate as the six anchored nits it did address.
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

	dc := vetting.DeliveryContext{
		GatePassed: true, ChatID: "chat-reanchor", CloneURL: "https://github.com/acme/widgets.git", IssueNumber: 7,
		Items: []vetting.StagedDelivery{{
			Kind: "review", Event: "request_changes", Body: "one blocker",
			Comments: []vetting.ReviewComment{{
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

// TestDeliverReviewKeepsUnanchorableFindingInBody pins #694's second case: a
// finding staged against a file whose diff hunk is pure deletion (no
// commentable RIGHT line at all) can't be re-anchored anywhere, so it must
// still reach the review body as a distinguishable, located item - not
// dropped, and not folded silently into prose.
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

	dc := vetting.DeliveryContext{
		GatePassed: true, ChatID: "chat-unanchorable", CloneURL: "https://github.com/acme/widgets.git", IssueNumber: 7,
		Items: []vetting.StagedDelivery{{
			Kind: "review", Event: "request_changes", Body: "one blocker",
			Comments: []vetting.ReviewComment{{Path: "dead.go", Line: 15, Body: "removed code still referenced elsewhere"}},
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

// TestDeliverReviewNeverPushesBranch pins #452: a review-only delivery with
// no PushedSHA (the gate never pushes a review-only node - vetting's own
// stagesPush guard, push_test.go) must never hit the push-verify endpoint.
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
	dc := vetting.DeliveryContext{
		GatePassed: true,
		ChatID:     "github-acme-widgets-7",
		CloneURL:   "https://github.com/acme/widgets.git",
		Branch:     "some-pr-branch",
		Items:      []vetting.StagedDelivery{{Kind: "review", Event: "approve", Body: "looks good"}},
	}
	if _, err := app.Deliver(context.Background(), dc); err != nil {
		t.Fatalf("review-only Deliver should succeed without any push: %v", err)
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

	dc := vetting.DeliveryContext{
		GatePassed:   false,
		GateFeedback: "tests fail",
		ChatID:       "github-acme-widgets-3",
		CloneURL:     "https://github.com/acme/widgets.git",
		Branch:       "quack/fix",
		Items:        []vetting.StagedDelivery{{Kind: "pull_request", Title: "Fix it", Body: "the fix"}},
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
	if !strings.Contains(posted.Body, "did NOT pass") {
		t.Fatalf("caveat banner missing from body: %s", posted.Body)
	}
	// #575: a fresh PR opened for a chat tied to issue #3 closes it deterministically.
	if !strings.Contains(posted.Body, "Closes #3") {
		t.Fatalf("delivered PR body missing deterministic Closes trailer: %s", posted.Body)
	}
}

// TestDeliverSuppressesClosesTrailerWhenPartialFix pins #575's "Done when":
// the quack:partial-fix label on the originating issue must suppress the
// deterministic trailer - a maintainer's explicit "this PR does not close it"
// signal, never overridden.
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
	dc := vetting.DeliveryContext{
		GatePassed: true,
		ChatID:     "github-acme-widgets-9",
		CloneURL:   "https://github.com/acme/widgets.git",
		Branch:     "quack/partial",
		Items:      []vetting.StagedDelivery{{Kind: "pull_request", Title: "Partial fix", Body: "does part of it"}},
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

// TestDeliverSkipsClosesTrailerWhenChatIDResolvesToAPR covers the edge case
// findOpenPR alone can't catch: a PR-scoped chat id (github-owner-repo-<PR
// number>) whose branch's ORIGINAL PR was since closed/merged also takes the
// fresh-open path (no OPEN PR on that branch), and the chat id's number is
// still a pull request, not an issue - GitHub's issues endpoint returns PRs
// too, so a body-less partial-fix check alone would wrongly close #92 by
// naming another pull request.
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
	dc := vetting.DeliveryContext{
		GatePassed: true,
		ChatID:     "github-acme-widgets-92",
		CloneURL:   "https://github.com/acme/widgets.git",
		Branch:     "quack/fix-92",
		Items:      []vetting.StagedDelivery{{Kind: "pull_request", Title: "Redo the fix", Body: "the fix"}},
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

// TestDeliverDoesNotAppendClosesOnPRUpdate pins the other half of #575: a PR
// update (an already-open PR on this branch - a fix/continuation run, not a
// fresh issue-implement run) must never get a Closes trailer, even when the
// chat id encodes a number - that number is the PR's own, not a distinct
// issue to close.
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
	dc := vetting.DeliveryContext{
		GatePassed: true,
		ChatID:     "github-acme-widgets-11", // same number as the PR itself
		CloneURL:   "https://github.com/acme/widgets.git",
		Branch:     "fix/11-something",
		Items:      []vetting.StagedDelivery{{Kind: "pull_request", Title: "Fix it", Body: "the fix"}},
	}
	if _, err := app.Deliver(context.Background(), dc); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if strings.Contains(patchedBody["body"], "Closes") {
		t.Fatalf("PR update must never gain a Closes trailer: %+v", patchedBody)
	}
}

// TestDeliverClosesTrailerNotDuplicated pins the other "Done when": a body
// that already references the issue with a closing keyword is left alone -
// no second trailer, and no partial-fix lookup needed to decide that.
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
	dc := vetting.DeliveryContext{
		GatePassed: true,
		ChatID:     "github-acme-widgets-5",
		CloneURL:   "https://github.com/acme/widgets.git",
		Branch:     "quack/fix5",
		Items:      []vetting.StagedDelivery{{Kind: "pull_request", Title: "Fix it", Body: "does the work.\n\nCloses #5\n"}},
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
