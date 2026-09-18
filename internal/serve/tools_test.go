package serve

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/cli"
	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/ledgertest"
	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/workspace"
)

// TestResolveToolNames guards the config-driven gating of runtime-conditional builtins,
// dropped silently rather than erroring - load_memory is gated exactly like recall_memory.
func TestResolveToolNames(t *testing.T) {
	cases := []struct {
		name             string
		configured       []string
		taskMemAvailable bool
		wantNames        []string
	}{
		{
			name:             "stage_memory present when task memory available",
			configured:       []string{"stage_memory"},
			taskMemAvailable: true,
			wantNames:        []string{"stage_memory"},
		},
		{
			name:             "stage_memory absent when task memory unavailable",
			configured:       []string{"stage_memory"},
			taskMemAvailable: false,
			wantNames:        []string{},
		},
		{
			name:             "load_memory present when task memory available",
			configured:       []string{"load_memory", "web_search"},
			taskMemAvailable: true,
			wantNames:        []string{"load_memory", "web_search"},
		},
		{
			name:             "load_memory absent when task memory unavailable",
			configured:       []string{"load_memory", "web_search"},
			taskMemAvailable: false,
			wantNames:        []string{"web_search"},
		},
		{
			name:             "load_memory then recall_memory collapses to the first",
			configured:       []string{"load_memory", "recall_memory", "web_search"},
			taskMemAvailable: true,
			wantNames:        []string{"load_memory", "web_search"},
		},
		{
			name:             "recall_memory then load_memory collapses to the first",
			configured:       []string{"recall_memory", "load_memory", "web_search"},
			taskMemAvailable: true,
			wantNames:        []string{"recall_memory", "web_search"},
		},
		{
			name:       "unrelated tools always pass through",
			configured: []string{"web_search", "web_fetch", "summarize", "current_date", "ask_user"},
			wantNames:  []string{"web_search", "web_fetch", "summarize", "current_date", "ask_user"},
		},
		{
			// Extension tool names (internal/github.App.Tools()) are not special-cased
			// here - they resolve later, in tools.Build, against Deps.ExtTools. An
			// agent gets one only by listing it, same as any builtin.
			name:       "extension tool names pass through unchanged",
			configured: []string{"read_file", "github_add_review_comment"},
			wantNames:  []string{"read_file", "github_add_review_comment"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotNames := resolveToolNames(tc.configured, tc.taskMemAvailable)
			if !reflect.DeepEqual(gotNames, tc.wantNames) {
				t.Errorf("names = %v, want %v", gotNames, tc.wantNames)
			}
		})
	}
}

// TestConfigListingBothMemoryToolsBuildsOne: a config naming both load_memory and
// recall_memory must build exactly one tool, so one call appends exactly one ledger entry.
func TestConfigListingBothMemoryToolsBuildsOne(t *testing.T) {
	ctx := context.Background()
	store, _ := newMemStoreForTest(t, "task")
	if _, err := store.Commit(ctx, memory.Scope{Role: "task"}, "explorer", memory.Provenance{}, []memory.Candidate{{Content: "the build uses bazel"}}, ""); err != nil {
		t.Fatalf("commit: %v", err)
	}

	names := resolveToolNames([]string{"load_memory", "recall_memory"}, true)
	lgr := ledgertest.NewMemStore()
	built, err := tools.Build(names, tools.Deps{Memory: store, Ledger: lgr, MemoryRole: "task"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(built) != 1 {
		t.Fatalf("built %d tools for %v, want exactly 1", len(built), names)
	}

	cs, ok := built[0].(ledger.CoordSetter)
	if !ok {
		t.Fatal("built tool does not implement ledger.CoordSetter")
	}
	cs.SetLedgerCoords(ledger.Coords{ChatID: "chat1", Node: "node1"})
	rt, ok := built[0].(interface {
		Run(ctx adkagent.Context, args any) (map[string]any, error)
	})
	if !ok {
		t.Fatal("built tool is not runnable")
	}
	if _, err := rt.Run(newFakeToolCtx(), map[string]any{"query": "build system"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	entries, err := lgr.ReadEntries(ctx, "chat1", 0)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	if len(entries) != 1 || entries[0].Kind != ledger.KindMemoryRecall {
		t.Fatalf("entries = %+v, want exactly one memory.recall", entries)
	}
}

// fakeToolCtx supplies a real Ctx (StrictContextMock panics without one) plus the
// identity fields the repeatGuard/emit wrapper chain reads to Run a built tool.
type fakeToolCtx struct {
	adkagent.StrictContextMock
}

func newFakeToolCtx() *fakeToolCtx {
	return &fakeToolCtx{StrictContextMock: adkagent.StrictContextMock{Ctx: context.Background()}}
}

func (c *fakeToolCtx) UserContent() *genai.Content                          { return nil }
func (c *fakeToolCtx) InvocationID() string                                 { return "inv" }
func (c *fakeToolCtx) AgentName() string                                    { return "test" }
func (c *fakeToolCtx) UserID() string                                       { return "u" }
func (c *fakeToolCtx) AppName() string                                      { return "app" }
func (c *fakeToolCtx) SessionID() string                                    { return "sess" }
func (c *fakeToolCtx) Session() session.Session                             { return nil }
func (c *fakeToolCtx) Branch() string                                       { return "" }
func (c *fakeToolCtx) ToolConfirmation() *toolconfirmation.ToolConfirmation { return nil }

// TestEmitServerConfigToolsBuild runs the `quack init` wizard's own output through the
// server's startup tool resolution (resolveToolNames + tools.Build): the wizard kept
// emitting the pre-ACP toolset (cd/git_clone/run_command/run_code; #343 deleted their
// constructors), so a fresh config died at boot with `unknown builtin tool "cd"`.
func TestEmitServerConfigToolsBuild(t *testing.T) {
	t.Setenv("QUACK_LLM_API_KEY", "k")
	a := cli.InitAnswers{
		Endpoint: "http://x/v1", MainModel: "m", JudgeModel: "j", EmbedModel: "e",
		SessionKind: "sqlite", MemoryKind: "sqlite",
		WebSearch: true, SearchKind: "exa", WebFetch: true, FetchKind: "direct",
		Coding: true, Sandbox: "none",
	}
	path := filepath.Join(t.TempDir(), "quack.yaml")
	if err := os.WriteFile(path, []byte(cli.EmitServerConfig(a)), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("emitted config failed to load: %v", err)
	}
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	deps := tools.Deps{
		WebSearch:       tools.Backend{Kind: cfg.Tools["web_search"].Kind, URL: cfg.Tools["web_search"].URL},
		Fetch:           tools.Backend{Kind: cfg.Tools["web_fetch"].Kind, URL: cfg.Tools["web_fetch"].URL},
		Summarizer:      directAnswerModel{},
		Workspace:       jail,
		WorkspaceUserID: "u",
	}
	for name, ac := range cfg.Agents {
		if ac.Acp != nil {
			continue // external worker: brings its own tools, quack builds none
		}
		names := resolveToolNames(ac.Tools, true)
		if _, err := tools.Build(names, deps); err != nil {
			t.Errorf("agent %q tools %v: %v", name, ac.Tools, err)
		}
	}
}
