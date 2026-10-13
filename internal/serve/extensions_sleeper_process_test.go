package serve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/extensions/sdk/extproc"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/runlog"
	"github.com/fagerbergj/quack/internal/server"
	"github.com/fagerbergj/quack/internal/server/rest"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/workflowcatalog"
)

// The owner's league and user that extensions/sleeper/testdata/get was recorded against.
const (
	sleeperLeague = "1356407594683482112"
	sleeperUser   = "860211057418018816"
)

// fakeSleeperAPI replays extensions/sleeper/testdata/get, keyed as cmd/qa-mock keys it.
func fakeSleeperAPI(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + "_" + r.URL.Path
		if r.URL.RawQuery != "" {
			key += "_" + r.URL.RawQuery
		}
		sum := sha256.Sum256([]byte(key))
		b, err := os.ReadFile(filepath.Join("..", "..", "extensions", "sleeper", "testdata", "get", hex.EncodeToString(sum[:8])+".json"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// decideRecorder is a decision handler that records each request body and answers yes.
type decideRecorder struct {
	mu     sync.Mutex
	bodies []string
}

func (d *decideRecorder) serve(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		d.mu.Lock()
		d.bodies = append(d.bodies, string(b))
		d.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"answers": map[string]any{"swap": map[string]any{"true": 0.8, "false": 0.2}}})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (d *decideRecorder) seen() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.bodies...)
}

// sleeperFlow is what a user, an agent and the decision handler observe of sleeper.
type sleeperFlow struct {
	seasons, runnable, page, artifacts string
	jobs                               []string // POST /sleeper/api/jobs replies
	chats                              []string // title, origin and turn count per dispatched chat
	toolDecl, toolResult               string
	decisions                          []string
}

// fetchedAt is a read's wall clock, the one field two runs never share.
var fetchedAt = regexp.MustCompile(`"fetched_at(_local)?":"[^"]*"`)

type sleeperInvocation struct{ agent.StrictContextMock }

func (*sleeperInvocation) Artifacts() agent.Artifacts { return nil }

// runSleeper boots extensions.sleeper from block against a stub-model orchestrator and a recorded Sleeper
// API, calls an agent tool, and runs a lineup job (whose done RunEnded asks the decision handler) and a
// trends job (which also dispatches season notes). child reports whether quack ran it as a child process.
func runSleeper(t *testing.T, block string) (f sleeperFlow, child bool) {
	t.Helper()
	st, orch, hub, artifacts, jail := newExtTestStack(t)
	var orchRef atomic.Pointer[orchestrator.Orchestrator]
	orchRef.Store(orch)
	var judgeModelRef atomic.Pointer[model.LLM]
	shapes := shapesRefOf([]workflowcatalog.Shape{{Name: "sleeper-lineup", Trigger: "t", DAGShape: "s"},
		{Name: "sleeper-trends", Trigger: "t", DAGShape: "s"}, {Name: "sleeper-season-notes", Trigger: "t", DAGShape: "s"}})
	rec := &decideRecorder{}
	cfg := noopModulesConfig(t, t.TempDir(), block)
	cfg.Decisions = decisionsCfg(rec.serve(t), map[string]config.DecisionPoint{"ext:sleeper/lineup_change": {Enabled: true, Mode: "observe"}})
	decisions := &extDecisions{}
	exts, err := buildSDKExtensions(cfg, st, hub, runlog.NewEventLog(st), &orchRef, artifacts, jail, &judgeModelRef, nil, nil, nil, shapes, decisions)
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
	ts := httptest.NewServer(server.New(server.Options{REST: &rest.Handler{}, SDKExtensions: sdkExtensionMounts(exts)}))
	t.Cleanup(ts.Close)
	_, child = exts[0].ext.(*extproc.Process)

	f.seasons = getBody(t, http.DefaultClient, ts.URL+"/sleeper/api/seasons?league_id="+sleeperLeague)
	f.runnable = getBody(t, http.DefaultClient, ts.URL+"/sleeper/api/jobs")
	f.page = getBody(t, http.DefaultClient, ts.URL+"/sleeper/")
	f.toolDecl, f.toolResult = callSleeperTool(t, exts)

	lineupChat := "ext:sleeper:" + sleeperLeague + ":3:lineup"
	lineup, err := os.ReadFile("../../extensions/sleeper/ui/fixtures/lineup.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := writeExtInputArtifact(st, artifacts)(lineupChat, sleeperUser, "lineup", "application/json", lineup); err != nil {
		t.Fatal(err)
	}
	for _, job := range []string{"lineup", "trends"} {
		f.jobs = append(f.jobs, postJob(t, ts.URL, job))
	}
	chats := []string{lineupChat, "ext:sleeper:" + sleeperLeague + ":3:trends", "ext:sleeper:" + sleeperLeague + ":season-notes"}
	for _, id := range chats {
		waitRunSettled(t, st, id)
	}
	waitUntil(t, 10*time.Second, func() bool {
		return len(rec.seen()) == 2 && !strings.Contains(getBody(t, http.DefaultClient, ts.URL+"/sleeper/api/artifacts?stop=3"), `"running":true`)
	})
	f.artifacts = getBody(t, http.DefaultClient, ts.URL+"/sleeper/api/artifacts?stop=3")
	f.decisions = rec.seen()
	for _, id := range chats {
		c, err := st.GetChat(context.Background(), id)
		if err != nil || c == nil {
			t.Fatalf("GetChat(%s) = %v, %v", id, c, err)
		}
		turns, _ := st.ListTurns(context.Background(), c.ID)
		f.chats = append(f.chats, c.Title+" "+c.Origin+" "+string(rune('0'+len(turns))))
	}
	return f, child
}

func postJob(t *testing.T, base, job string) string {
	t.Helper()
	resp, err := http.Post(base+"/sleeper/api/jobs", "application/json", strings.NewReader(`{"stop":"3","job":"`+job+`"}`))
	if err != nil {
		t.Fatalf("POST %s job: %v", job, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.Status + " " + string(b)
}

// callSleeperTool runs sleeper_standings as a DAG node's agent gets it: indexed from the extension's Tools()
// and built by quack's tool registry, which stamps the node's CallInfo on the call.
func callSleeperTool(t *testing.T, exts []builtSDKExtension) (decl, result string) {
	t.Helper()
	built, err := tools.Build([]string{"sleeper_standings"}, tools.Deps{ExtTools: indexExtTools(sdkExtensionTools(exts)),
		CallScope: tools.CallScope{AdvisorToken: "unknown-node", ChatID: "ext:sleeper:c", UserID: sleeperUser}})
	if err != nil {
		t.Fatal(err)
	}
	tl := built[0].(renamableTool)
	d, _ := json.Marshal(tl.Declaration())
	ctx := agent.NewToolContext(&sleeperInvocation{agent.NewStrictContextMock(context.Background())}, "call-1", nil, nil)
	res, err := tl.Run(ctx, map[string]any{})
	if err != nil {
		t.Fatalf("sleeper_standings: %v", err)
	}
	r, _ := json.Marshal(res)
	return string(d), fetchedAt.ReplaceAllString(string(r), `"fetched_at":""`)
}

// TestSleeperOutOfProcessMatchesInProcess: with exec set, sleeper's routes, an agent tool, its jobs' Dispatch,
// RunEnded, ReadArtifact and Decide all cross the process boundary and match the compiled-in extension.
func TestSleeperOutOfProcessMatchesInProcess(t *testing.T) {
	bin := buildExtChild(t, "sleeper")
	block := "sleeper:\n  base_url: " + fakeSleeperAPI(t) + "\n  default_user: " + sleeperUser + "\n  default_league: " + sleeperLeague + "\n"

	want, _ := runSleeper(t, block)
	got, child := runSleeper(t, block+"  exec: "+bin+"\n")
	if !child {
		t.Fatal("exec did not run sleeper as a child process")
	}

	if !strings.Contains(want.seasons, sleeperLeague) || !strings.Contains(want.toolResult, `"standings":[{`) ||
		!strings.Contains(want.artifacts, `"lineup":{"found":true`) || len(want.decisions) != 2 ||
		!strings.Contains(want.decisions[0], `"slot":"RB2"`) || !strings.HasPrefix(want.jobs[1], "200 ") {
		t.Errorf("in-process flow = %+v, want seasons, standings, a found lineup, two lineup decisions and accepted jobs", want)
	}
	for _, c := range []struct{ name, got, want string }{
		{"seasons", got.seasons, want.seasons}, {"runnable jobs", got.runnable, want.runnable}, {"page", got.page, want.page},
		{"artifacts", got.artifacts, want.artifacts}, {"tool declaration", got.toolDecl, want.toolDecl},
		{"tool result", got.toolResult, want.toolResult}, {"jobs", strings.Join(got.jobs, "\n"), strings.Join(want.jobs, "\n")},
		{"chats", strings.Join(got.chats, "\n"), strings.Join(want.chats, "\n")},
		{"decisions", strings.Join(got.decisions, "\n"), strings.Join(want.decisions, "\n")},
	} {
		if c.got != c.want {
			t.Errorf("%s out of process:\n%s\nin process:\n%s", c.name, c.got, c.want)
		}
	}
}
