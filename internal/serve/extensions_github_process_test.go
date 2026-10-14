package serve

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"

	gh "github.com/fagerbergj/quack/extensions/github"
	"github.com/fagerbergj/quack/extensions/sdk/extproc"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/server"
	"github.com/fagerbergj/quack/internal/server/rest"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/vetting"
)

const (
	ghChat      = "ext:github:github-acme-widgets-7"
	ghToken     = "ghs_e2e_installation_token"
	ghSecret    = "e2e-webhook-secret"
	ghHeadSHA   = "head2222222222222222222222222222222222222"
	ghBaseSHA   = "base1111111111111111111111111111111111111"
	ghRepoURL   = "https://github.com/acme/widgets"
	ghPRPayload = `{"action":"%s","number":7,"pull_request":{"title":"Fix the retry loop","head":{"sha":"` + ghHeadSHA + `"},` +
		`"state":"%s","merged":false},"label":{"name":"quack-auto-review"},"repository":{"name":"widgets","full_name":"acme/widgets",` +
		`"owner":{"login":"acme"},"clone_url":"https://github.com/acme/widgets.git","default_branch":"main"},` +
		`"sender":{"login":"alice"},"installation":{"id":5}}`
)

// ghGets are the GitHub API reads the label-review flow makes, by path; lists not named here are empty.
var ghGets = map[string]string{
	"/repos/acme/widgets/installation": `{"id":5}`,
	"/app":                             `{"slug":"quack-e2e"}`,
	"/repos/acme/widgets/pulls/7": `{"number":7,"title":"Fix the retry loop","body":"Retries forever.","state":"open","user":{"login":"alice"},` +
		`"head":{"ref":"feature","sha":"` + ghHeadSHA + `","repo":{"full_name":"acme/widgets","fork":false}},` +
		`"base":{"ref":"main","sha":"` + ghBaseSHA + `","repo":{"full_name":"acme/widgets"}},"labels":[{"name":"quack-auto-review"}]}`,
	"/repos/acme/widgets/issues/7": `{"number":7,"title":"Fix the retry loop","body":"Retries forever.","state":"open","user":{"login":"alice"},` +
		`"pull_request":{"url":"x"},"labels":[{"name":"quack-auto-review"}]}`,
	"/repos/acme/widgets/git/ref/heads/main":                     `{"object":{"sha":"` + ghBaseSHA + `"}}`,
	"/repos/acme/widgets/git/ref/heads/feature":                  `{"object":{"sha":"` + ghHeadSHA + `"}}`,
	"/repos/acme/widgets/pulls/7/files":                          `[{"filename":"retry.go","status":"modified","patch":"@@ -1,3 +1,4 @@\n package retry\n+// loop\n func A() {}\n func B() {}"}]`,
	"/repos/acme/widgets/commits/" + ghHeadSHA:                   `{"sha":"` + ghHeadSHA + `","author":{"login":"alice"},"commit":{"message":"fix"}}`,
	"/repos/acme/widgets/commits/" + ghHeadSHA + "/check-runs":   `{"total_count":0,"check_runs":[]}`,
	"/repos/acme/widgets/commits/" + ghHeadSHA + "/check-suites": `{"total_count":0,"check_suites":[]}`,
}

// fakeGitHub serves ghGets, mints ghToken, and records every write GitHub would see.
type fakeGitHub struct {
	mu     sync.Mutex
	writes []string
}

func (f *fakeGitHub) serve(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens") {
			_, _ = fmt.Fprintf(w, `{"token":%q,"expires_at":"2099-01-01T00:00:00Z"}`, ghToken)
			return
		}
		if r.Method == http.MethodGet {
			body, ok := ghGets[r.URL.Path]
			switch {
			case ok:
				_, _ = io.WriteString(w, body)
			case strings.HasSuffix(r.URL.Path, "s") || strings.HasSuffix(r.URL.Path, "timeline"):
				_, _ = io.WriteString(w, `[]`)
			default:
				http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			}
			return
		}
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.writes = append(f.writes, r.Method+" "+r.URL.Path+" "+string(b))
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":1,"number":7,"html_url":"https://github.com/acme/widgets/pull/7#w"}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (f *fakeGitHub) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

// sortedFrom returns the writes from the nth on in a stable order: the flow's goroutines race each other to GitHub.
func (f *fakeGitHub) sortedFrom(n int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Sorted(slices.Values(f.writes[n:]))
}

// gatedModel answers like directAnswerModel once release closes, so a test can act while the run is live.
type gatedModel struct{ release chan struct{} }

func (gatedModel) Name() string { return "gated-stub" }

func (m gatedModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		select {
		case <-m.release:
		case <-ctx.Done():
			return
		}
		for r, err := range (directAnswerModel{}).GenerateContent(ctx, req, stream) {
			if !yield(r, err) {
				return
			}
		}
	}
}

// nodeInvocation is the agent ctx an assignment hook receives; only its ctx and session id answer.
type nodeInvocation struct {
	agent.StrictContextMock
	session string
}

func (n *nodeInvocation) SessionID() string { return n.session }

// githubFlow is what GitHub, the user and quack's runner observe of the github extension.
type githubFlow struct {
	unsigned, labeled, closed string
	meta                      string
	chat, artifacts           string
	ranWrites                 []string
	delivered, deliverWrites  []string
	credential, fresh         string
	decisions                 []string
}

func writePEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "app-key-secret.pem")
	data := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func postWebhook(t *testing.T, url, event, body, secret string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", "d-1")
	if secret != "" {
		req.Header.Set("X-Hub-Signature-256", gh.SignWebhookBody([]byte(secret), []byte(body)))
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.Status + " " + strings.TrimSpace(string(b))
}

// runGithub boots extensions.github from block: a signed label webhook dispatches a review run, which stamps
// assignment meta while live and finishes through RunEnded; then quack pushes a review through Deliver, asks
// for a git credential and a freshness check, and a closed webhook refreshes the chat's origin.
func runGithub(t *testing.T, block string) (f githubFlow, child bool) {
	t.Helper()
	release := make(chan struct{})
	st, orch, hub, artifacts, jail := newExtTestStackWithModel(t, gatedModel{release: release})
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var judgeModelRef atomic.Pointer[model.LLM]
	fake := &fakeGitHub{}
	rec := &decideRecorder{}
	cfg := noopModulesConfig(t, t.TempDir(), block+"  api_base: "+fake.serve(t)+"\n")
	cfg.Decisions = decisionsCfg(rec.serve(t), map[string]config.DecisionPoint{
		"ext:github/review.verdict": {Enabled: true, Mode: "observe"}, "ext:github/finding.blocking": {Enabled: true, Mode: "observe"}})
	decisions := &extDecisions{}
	exts, err := buildSDKExtensions(cfg, st, hub, runlog.NewEventLog(st), &orchRef, artifacts, jail, &judgeModelRef, nil, nil, nil, nil, decisions, nil)
	if err != nil || len(exts) != 1 {
		t.Fatalf("buildSDKExtensions = %d exts, %v", len(exts), err)
	}
	t.Cleanup(func() { closeSDKExtensions(exts) })
	if _, err := decisions.build(cfg.Decisions); err != nil {
		t.Fatal(err)
	}
	if err := startSDKExtensions(context.Background(), exts); err != nil {
		t.Fatal(err)
	}
	gitTokens, deliver, freshness, assignmentMeta := discoverSDKToolSources(exts)
	ts := httptest.NewServer(server.New(server.Options{REST: &rest.Handler{}, SDKExtensions: sdkExtensionMounts(exts)}))
	t.Cleanup(ts.Close)
	_, child = exts[0].ext.(*extproc.Process)
	hook := ts.URL + "/github/webhook"

	f.unsigned = postWebhook(t, hook, "pull_request", fmt.Sprintf(ghPRPayload, "labeled", "open"), "")
	f.labeled = postWebhook(t, hook, "pull_request", fmt.Sprintf(ghPRPayload, "labeled", "open"), ghSecret)
	node := &nodeInvocation{agent.NewStrictContextMock(context.Background()), ghChat}
	waitUntil(t, 10*time.Second, func() bool {
		_, meta := assignmentMeta(node, "plan-1", "code-reviewer", dag.Assignment{NodeID: "review-1"})
		f.meta = fmt.Sprint(meta)
		return meta != nil
	})
	close(release)
	waitRunSettled(t, st, ghChat)
	waitUntil(t, 10*time.Second, func() bool { return fake.count() > 0 }) // finalize's one write: the no-verdict comment
	f.ranWrites = fake.sortedFrom(0)
	f.chat, f.artifacts = githubChat(t, st, artifacts)

	f.delivered, f.deliverWrites = deliverReview(t, deliver, fake)
	f.credential = credentialFor(t, gitTokens)
	for _, base := range []string{ghBaseSHA, "0ld"} {
		fresh, reason := freshness(node, "plan-1", "code-reviewer", "ctx-1", dag.Assignment{NodeID: "review-1",
			Meta: map[string]map[string]any{"github": {"repo": ghRepoURL, "base_ref": "main", "base_sha": base}}})
		f.fresh += fmt.Sprintf("%t %s; ", fresh, reason)
	}
	f.closed = postWebhook(t, hook, "pull_request", strings.Replace(fmt.Sprintf(ghPRPayload, "closed", "closed"), `"merged":false`, `"merged":true`, 1), ghSecret)
	waitUntil(t, 5*time.Second, func() bool {
		c, _ := st.GetChat(context.Background(), ghChat)
		return c != nil && strings.Contains(c.Origin, `"merged"`)
	})
	c, _ := st.GetChat(context.Background(), ghChat)
	f.chat += "\nafter close: " + c.Origin
	f.decisions = rec.seen()
	return f, child
}

// githubChat is the dispatched chat as the sidebar and runner see it, and the input artifacts dispatch wrote.
func githubChat(t *testing.T, st *store.Store, artifacts *store.TurnAwareService) (chat, inputs string) {
	t.Helper()
	c, err := st.GetChat(context.Background(), ghChat)
	if err != nil || c == nil {
		t.Fatalf("GetChat(%s) = %v, %v", ghChat, c, err)
	}
	turns, _ := st.ListTurns(context.Background(), c.ID)
	chat = fmt.Sprintf("%s | %s | %s | %d turns | %s", c.Title, c.SessionUser, c.RunStatus, len(turns), c.Origin)
	read := readExtInputArtifact(st, artifacts)
	for _, name := range []string{"event", "issue", "pull", "files", "comments"} {
		data, ok := read(ghChat, c.SessionUser, name)
		inputs += fmt.Sprintf("%s %t %s\n", name, ok, data)
	}
	return chat, inputs
}

// deliverReview hands the extension a gated review as quack's vetting gate does, returning the outcomes and the writes.
func deliverReview(t *testing.T, deliver vetting.DeliverFunc, fake *fakeGitHub) (outcomes, writes []string) {
	t.Helper()
	n := fake.count()
	out, err := deliver(context.Background(), vetting.DeliveryContext{NodeID: "review-1", ChatID: ghChat, CloneURL: ghRepoURL,
		Branch: "feature", IssueNumber: 7, GatePassed: true, IdempotencyKey: "code_review:pr:7@1",
		Items: []vetting.StagedDelivery{{Kind: "review", Event: "comment", Body: "One finding.",
			Comments: []vetting.ReviewComment{{Path: "retry.go", Line: 2, Body: "blocking: this loops forever", Severity: "blocking"}}}}})
	for _, o := range out {
		outcomes = append(outcomes, fmt.Sprintf("%+v", o))
	}
	return append(outcomes, fmt.Sprint(err)), fake.sortedFrom(n)
}

func credentialFor(t *testing.T, src tools.GitTokenSource) string {
	t.Helper()
	var out []string
	for _, u := range []string{ghRepoURL + ".git", "https://gitlab.com/acme/widgets.git"} {
		c, err := src.GitCredential(context.Background(), u)
		out = append(out, fmt.Sprintf("%+v %v", c, err))
	}
	return strings.Join(out, "; ")
}

// lockedBuffer collects quack's log, the child's forwarded lines included, across goroutines.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestGithubOutOfProcessMatchesInProcess: with exec set, github's public webhook (signature checked in the child),
// Dispatch, WriteArtifact, assignment meta and freshness, RunEnded, Deliver with its decisions, the git credential and
// UpdateChatOrigin all cross the process boundary and match the compiled-in extension. Neither run logs the
// installation token or the App key's path.
func TestGithubOutOfProcessMatchesInProcess(t *testing.T) {
	logs := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	bin := buildExtChild(t, "github")
	keyPath := writePEM(t)
	block := "github:\n  client_id: Iv1.e2e\n  private_key_path: " + keyPath + "\n  webhook_secret: " + ghSecret +
		"\n  triggers: [label]\n  allowed_users: [alice]\n"

	want, _ := runGithub(t, block)
	got, child := runGithub(t, block+"  exec: "+bin+"\n")
	if !child {
		t.Fatal("exec did not run github as a child process")
	}

	if !strings.HasPrefix(want.unsigned, "401 ") || !strings.HasPrefix(want.labeled, "202 ") || !strings.Contains(want.meta, ghHeadSHA) ||
		!strings.Contains(want.artifacts, "event true") || !strings.Contains(strings.Join(want.deliverWrites, "\n"), "/pulls/7/reviews") ||
		!strings.Contains(want.credential, ghToken) || !strings.HasPrefix(want.fresh, "true ; false base moved") || len(want.decisions) == 0 {
		t.Errorf("in-process flow = %+v, want a 401 unsigned and 202 signed webhook, assignment meta, input artifacts, "+
			"a posted review, a token, fresh then stale, and decisions", want)
	}
	for _, c := range []struct{ name, got, want string }{
		{"unsigned webhook", got.unsigned, want.unsigned}, {"signed webhook", got.labeled, want.labeled},
		{"closed webhook", got.closed, want.closed}, {"assignment meta", got.meta, want.meta},
		{"chat", got.chat, want.chat}, {"input artifacts", got.artifacts, want.artifacts},
		{"run's GitHub writes", strings.Join(got.ranWrites, "\n"), strings.Join(want.ranWrites, "\n")},
		{"delivery outcomes", strings.Join(got.delivered, "\n"), strings.Join(want.delivered, "\n")},
		{"delivery writes", strings.Join(got.deliverWrites, "\n"), strings.Join(want.deliverWrites, "\n")},
		{"git credential", got.credential, want.credential}, {"freshness", got.fresh, want.fresh},
		{"decisions", strings.Join(got.decisions, "\n"), strings.Join(want.decisions, "\n")},
	} {
		if c.got != c.want {
			t.Errorf("%s out of process:\n%s\nin process:\n%s", c.name, c.got, c.want)
		}
	}
	if l := logs.String(); strings.Contains(l, ghToken) || strings.Contains(l, keyPath) {
		t.Error("quack's log carries the installation token or the App key's path")
	}
}
