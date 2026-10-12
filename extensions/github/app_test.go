package github

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"

	"github.com/fagerbergj/quack-extensions/github/internal/httpx"
	"github.com/fagerbergj/quack-extensions/sdk"
)

// testKeyPEM is defined in webhook_test.go (same package).

func TestAppJWTClaims(t *testing.T) {
	// The issuer is passed through verbatim as `iss` - GitHub accepts either a
	// stringified App ID (legacy) or a Client ID (recommended).
	for _, issuer := range []string{"424242", "Iv23liAbCdEfGhIjKlMn"} {
		t.Run(issuer, func(t *testing.T) {
			keyPEM, pub := testKeyPEM(t)
			app, err := NewApp(issuer, keyPEM)
			if err != nil {
				t.Fatalf("NewApp: %v", err)
			}
			tokStr, err := app.appJWT()
			if err != nil {
				t.Fatalf("appJWT: %v", err)
			}
			var claims jwt.RegisteredClaims
			_, err = jwt.ParseWithClaims(tokStr, &claims, func(*jwt.Token) (any, error) { return pub, nil })
			if err != nil {
				t.Fatalf("parse jwt: %v", err)
			}
			if claims.Issuer != issuer {
				t.Errorf("iss = %q; want %q", claims.Issuer, issuer)
			}
			// exp must be in the future and at most 10 minutes out (GitHub's cap).
			d := time.Until(claims.ExpiresAt.Time)
			if d <= 0 || d > 10*time.Minute {
				t.Errorf("exp in %s; want (0, 10m]", d)
			}
			// iat backdated to tolerate clock skew.
			if !claims.IssuedAt.Time.Before(time.Now()) {
				t.Errorf("iat = %v; want backdated", claims.IssuedAt.Time)
			}
		})
	}
}

func TestNewAppRejectsBadKey(t *testing.T) {
	if _, err := NewApp("1", "not a pem"); err == nil {
		t.Fatal("expected error for a bad private key")
	}
}

func TestInstallationTokenCaching(t *testing.T) {
	keyPEM, _ := testKeyPEM(t)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/app/installations/99/access_tokens" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		atomic.AddInt32(&hits, 1)
		fmt.Fprintf(w, `{"token":"ghs_secret","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	}))
	defer srv.Close()

	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.apiBase = srv.URL

	for i := 0; i < 3; i++ {
		tok, err := app.InstallationToken(context.Background(), 99)
		if err != nil {
			t.Fatalf("InstallationToken: %v", err)
		}
		if tok != "ghs_secret" {
			t.Fatalf("token = %q; want ghs_secret", tok)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("token endpoint hit %d times; want 1 (cached)", got)
	}
}

func TestInstallationForRepoCaching(t *testing.T) {
	keyPEM, _ := testKeyPEM(t)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widgets/installation" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		atomic.AddInt32(&hits, 1)
		fmt.Fprint(w, `{"id":777}`)
	}))
	defer srv.Close()

	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.apiBase = srv.URL

	for i := 0; i < 2; i++ {
		id, err := app.InstallationForRepo(context.Background(), "acme", "widgets")
		if err != nil {
			t.Fatalf("InstallationForRepo: %v", err)
		}
		if id != 777 {
			t.Fatalf("id = %d; want 777", id)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("installation endpoint hit %d times; want 1 (cached)", got)
	}
}

func TestListReviews(t *testing.T) {
	keyPEM, _ := testKeyPEM(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/reviews"):
			fmt.Fprint(w, `[
				{"commit_id":"aaa111","user":{"login":"alice"},"submitted_at":"2026-07-01T00:00:00Z"},
				{"commit_id":"bbb222","user":{"login":"quack[bot]"},"submitted_at":"2026-07-02T00:00:00Z"}
			]`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.apiBase = srv.URL

	reviews, err := app.listReviews(context.Background(), "acme", "widgets", 7)
	if err != nil {
		t.Fatalf("listReviews: %v", err)
	}
	if len(reviews) != 2 {
		t.Fatalf("len(reviews) = %d; want 2", len(reviews))
	}
	if reviews[0].CommitID != "aaa111" || reviews[0].User.Login != "alice" {
		t.Errorf("reviews[0] = %+v", reviews[0])
	}
	if reviews[1].CommitID != "bbb222" || reviews[1].User.Login != "quack[bot]" {
		t.Errorf("reviews[1] = %+v", reviews[1])
	}
}

// pullMeta's Fork detection feeds computeGrant's fork check: a wrong read here
// silently over-grants pull_request on a fork PR quack cannot push to.
func TestPullMetaDetectsFork(t *testing.T) {
	keyPEM, _ := testKeyPEM(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/7"):
			fmt.Fprint(w, `{"title":"t","body":"","state":"open",
				"head":{"ref":"feature","sha":"h1","repo":{"full_name":"mallory/widgets"}},
				"base":{"ref":"main","repo":{"full_name":"acme/widgets"}},"labels":[]}`)
		case strings.HasSuffix(r.URL.Path, "/8"):
			fmt.Fprint(w, `{"title":"t","body":"","state":"open",
				"head":{"ref":"feature","sha":"h1","repo":{"full_name":"acme/widgets"}},
				"base":{"ref":"main","repo":{"full_name":"acme/widgets"}},"labels":[]}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.apiBase = srv.URL

	fork, err := app.pullMeta(context.Background(), "acme", "widgets", 7)
	if err != nil {
		t.Fatalf("pullMeta: %v", err)
	}
	if !fork.Fork {
		t.Errorf("fork PR (head mallory/widgets != base acme/widgets): Fork = false, want true")
	}

	sameRepo, err := app.pullMeta(context.Background(), "acme", "widgets", 8)
	if err != nil {
		t.Fatalf("pullMeta: %v", err)
	}
	if sameRepo.Fork {
		t.Errorf("same-repo PR: Fork = true, want false")
	}
}

// fastResilientClient uses production's method-aware retry policy (internal/httpx) without the real backoff delay.
func fastResilientClient() *http.Client {
	return &http.Client{Transport: httpx.NewTransport(nil, time.Millisecond, 5*time.Millisecond)}
}

// A GET hitting a transient 503 ("no server available") succeeds on retry instead of failing the whole call.
func TestDoJSONRetriesGETOn503(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			http.Error(w, `{"message":"No server is currently available to service your request."}`, http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer srv.Close()

	app := &App{apiBase: srv.URL, http: fastResilientClient()}
	var out struct {
		OK bool `json:"ok"`
	}
	if err := app.doJSON(context.Background(), http.MethodGet, "/whatever", "token x", nil, &out); err != nil {
		t.Fatalf("doJSON: %v", err)
	}
	if !out.OK {
		t.Errorf("out.OK = false; want true")
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("server hit %d times; want 2 (one 503, one retry that succeeds)", got)
	}
}

// A POST that 503s is tried exactly once: retrying a mutating call risks a duplicate (e.g. a comment posted twice).
func TestDoJSONDoesNotRetryPOST(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, `{"message":"No server is currently available to service your request."}`, http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	app := &App{apiBase: srv.URL, http: fastResilientClient()}
	err := app.doJSON(context.Background(), http.MethodPost, "/whatever", "token x", map[string]string{"body": "hi"}, nil)
	if err == nil {
		t.Fatal("doJSON: expected error, got nil")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("server hit %d times; want 1 (POST must not be retried)", got)
	}
}

// GitHub's git-refs API isn't read-your-writes consistent, so a ref lookup right after an accepted push
// can 404; verifyPushedBranch must retry that 404 and return the SHA once it appears.
func TestVerifyPushedBranchRetries404ThenSucceeds(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) <= 2 {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `{"object":{"sha":"deadbeefcafef00d"}}`)
	}))
	defer srv.Close()

	app := &App{apiBase: srv.URL, http: srv.Client(), installs: map[string]int64{"acme/widgets": 1}, tokens: map[int64]cachedToken{1: {token: "ghs_x", expires: time.Now().Add(time.Hour)}}}
	sha, err := app.verifyPushedBranch(context.Background(), "acme", "widgets", "feature")
	if err != nil {
		t.Fatalf("verifyPushedBranch: %v", err)
	}
	if sha != "deadbeefcafef00d" {
		t.Errorf("sha = %q, want the SHA from the eventually-successful GET", sha)
	}
	if got := atomic.LoadInt32(&hits); got != 3 {
		t.Errorf("server hit %d times; want 3 (two 404s, one that finally lands)", got)
	}
}

// A ref that never appears must still fail loud: retrying can't turn a phantom push into a false success.
func TestVerifyPushedBranchPersistentNotFoundFails(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	app := &App{apiBase: srv.URL, http: srv.Client(), installs: map[string]int64{"acme/widgets": 1}, tokens: map[int64]cachedToken{1: {token: "ghs_x", expires: time.Now().Add(time.Hour)}}}
	_, err := app.verifyPushedBranch(context.Background(), "acme", "widgets", "feature")
	if err == nil {
		t.Fatal("verifyPushedBranch: expected an error for a ref that never appears, got nil")
	}
	if !strings.Contains(err.Error(), "status 404") {
		t.Errorf("error = %v; want it to carry the underlying 404", err)
	}
	if got := atomic.LoadInt32(&hits); got != pushVerifyAttempts {
		t.Errorf("server hit %d times; want %d (pushVerifyAttempts)", got, pushVerifyAttempts)
	}
}

// The 404-only retry must not paper over other failures (auth, exhausted 5xx); those return on the first attempt.
func TestVerifyPushedBranchDoesNotRetryOtherFailures(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()

	app := &App{apiBase: srv.URL, http: srv.Client(), installs: map[string]int64{"acme/widgets": 1}, tokens: map[int64]cachedToken{1: {token: "ghs_x", expires: time.Now().Add(time.Hour)}}}
	_, err := app.verifyPushedBranch(context.Background(), "acme", "widgets", "feature")
	if err == nil {
		t.Fatal("verifyPushedBranch: expected an error, got nil")
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("server hit %d times; want 1 (a non-404 failure must not retry)", got)
	}
}

func TestOwnerRepoFromURL(t *testing.T) {
	tests := []struct {
		url         string
		owner, repo string
		ok          bool
	}{
		{"https://github.com/acme/widgets.git", "acme", "widgets", true},
		{"https://github.com/acme/widgets", "acme", "widgets", true},
		{"https://gitlab.com/acme/widgets.git", "", "", false},
		{"https://github.com/acme", "", "", false},
		{"not a url ::", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			owner, repo, ok := ownerRepoFromURL(tt.url)
			if ok != tt.ok || owner != tt.owner || repo != tt.repo {
				t.Errorf("got (%q,%q,%v); want (%q,%q,%v)", owner, repo, ok, tt.owner, tt.repo, tt.ok)
			}
		})
	}
}

// TestCreatePullRequestAndAddLabels pins the PR-number plumbing the labels
// param of github_pull_request depends on.
func TestCreatePullRequestAndAddLabels(t *testing.T) {
	labeled := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			fmt.Fprint(w, `{"html_url":"https://github.com/acme/widgets/pull/42","number":42}`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues/42/labels"):
			body, _ := io.ReadAll(r.Body)
			labeled <- string(body)
			fmt.Fprint(w, `[]`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	keyPEM, _ := testKeyPEM(t)
	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.apiBase = srv.URL

	url, number, err := app.createPullRequest(context.Background(), "acme", "widgets", "t", "head", "main", "b", false)
	if err != nil {
		t.Fatalf("createPullRequest: %v", err)
	}
	if number != 42 || url == "" {
		t.Fatalf("createPullRequest = (%q, %d)", url, number)
	}
	if err := app.addLabels(context.Background(), "acme", "widgets", number, []string{"quack:review"}); err != nil {
		t.Fatalf("addLabels: %v", err)
	}
	if got := <-labeled; !strings.Contains(got, "quack:review") {
		t.Errorf("labels request body = %q", got)
	}
}

// A failed labels API call must surface as an error - a PR that opens but never
// gets its review label would silently stall the plan→implement→review chain.
func TestAddLabelsAPIErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":5}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
			http.Error(w, `{"message":"Validation Failed"}`, http.StatusUnprocessableEntity)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	keyPEM, _ := testKeyPEM(t)
	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	app.apiBase = srv.URL

	if err := app.addLabels(context.Background(), "acme", "widgets", 42, []string{"quack:review"}); err == nil {
		t.Fatal("addLabels: expected error on 422 response, got nil")
	}
}

// A failing gate prepends a visible warning (with feedback) to the delivered body; a passing gate leaves it untouched.
func TestGateCaveat(t *testing.T) {
	body := "## What\nadds a thing"
	if got := gateCaveat(sdk.DeliveryContext{GatePassed: true}, body); got != body {
		t.Errorf("passing gate must not alter the body; got %q", got)
	}
	got := gateCaveat(sdk.DeliveryContext{GatePassed: false, GateFeedback: "no test for the nil case"}, body)
	if !strings.Contains(got, "[!WARNING]") || !strings.Contains(got, "did not pass") {
		t.Errorf("failing gate must prepend a warning banner; got %q", got)
	}
	if !strings.Contains(got, "no test for the nil case") {
		t.Errorf("banner must carry the judge feedback; got %q", got)
	}
	if !strings.HasSuffix(got, body) {
		t.Errorf("banner must precede the original body; got %q", got)
	}
}

// A node that passed the gate but ran no build/test check gets a plain NOTE, not a warning about the code,
// carrying the skip reason verbatim.
func TestGateCaveatChecksSkipNote(t *testing.T) {
	body := "## What\nadds a thing"
	note := "quack did not run a build/test check on this change (skip_reason: unsupported_build_system)."
	got := gateCaveat(sdk.DeliveryContext{GatePassed: true, ChecksSkipNote: note}, body)
	if !strings.Contains(got, "[!NOTE]") {
		t.Errorf("a passing gate with a checks-skip note must prepend a NOTE banner; got %q", got)
	}
	if strings.Contains(got, "[!WARNING]") || strings.Contains(got, "did not pass") {
		t.Errorf("the note must not read as a gate-failure warning about the code; got %q", got)
	}
	if !strings.Contains(got, "unsupported_build_system") {
		t.Errorf("note must carry the exact skip reason; got %q", got)
	}
	if !strings.HasSuffix(got, body) {
		t.Errorf("note must precede the original body; got %q", got)
	}
}

// A failing node's warning banner is unchanged by ChecksSkipNote.
func TestGateCaveatFailingNodeIgnoresChecksSkipNote(t *testing.T) {
	body := "## What\nadds a thing"
	dc := sdk.DeliveryContext{GatePassed: false, GateFeedback: "tests fail", ChecksSkipNote: "unsupported_build_system"}
	got := gateCaveat(dc, body)
	if strings.Contains(got, "[!NOTE]") || strings.Contains(got, "unsupported_build_system") {
		t.Errorf("a failing node's banner must ignore ChecksSkipNote entirely; got %q", got)
	}
	if !strings.Contains(got, "[!WARNING]") || !strings.Contains(got, "did not pass") || !strings.Contains(got, "tests fail") {
		t.Errorf("the existing failing-gate banner must be unchanged; got %q", got)
	}
}

// client_id is the only issuer: a config still using the removed app_id fails to load.
func TestFactoryRequiresClientID(t *testing.T) {
	_, err := factory(sdk.Host{DataDir: t.TempDir()}, []byte("app_id: 1\nprivate_key: k\nwebhook_secret: s\n"))
	if err == nil || !strings.Contains(err.Error(), "client_id is required") {
		t.Fatalf("factory err = %v, want client_id is required", err)
	}
}

func TestEnrichFailingChecksCapsWhyAtRunes(t *testing.T) {
	keyPEM, _ := testKeyPEM(t)
	long := strings.Repeat("é", 300)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/installation"):
			fmt.Fprint(w, `{"id":99}`)
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"t","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/check-runs/7/annotations"):
			fmt.Fprintf(w, `[{"path":"a.go","start_line":3,"annotation_level":"failure","message":%q}]`, long)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	app, err := NewApp("1", keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	app.apiBase = srv.URL

	runs := []checkRunView{{ID: 7, Conclusion: "failure"}}
	app.enrichFailingChecks(context.Background(), "o", "r", runs)
	if len(runs[0].Why) != 1 {
		t.Fatalf("Why = %v, want one line", runs[0].Why)
	}
	why := runs[0].Why[0]
	if n := utf8.RuneCountInString(why); n != 201 || !strings.HasSuffix(why, "…") || !utf8.ValidString(why) {
		t.Fatalf("Why = %d runes, valid=%v, suffix ellipsis=%v", n, utf8.ValidString(why), strings.HasSuffix(why, "…"))
	}
}
