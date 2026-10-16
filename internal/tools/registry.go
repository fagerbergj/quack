package tools

import (
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/httpx"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/workspace"
)

// runnableTool is a function tool whose Run a wrapper can intercept.
type runnableTool interface {
	tool.Tool
	Declaration() *genai.FunctionDeclaration
	Run(ctx agent.Context, args any) (map[string]any, error)
	ProcessRequest(ctx agent.Context, req *model.LLMRequest) error
}

type Deps struct {
	Client          *http.Client
	Guarded         *http.Client
	WebSearch       Backend
	Fetch           Backend
	Summarizer      model.LLM
	Cache           *URLCache
	Workspace       *workspace.Jail
	WorkspaceUserID string
	WorkspaceCaps   workspace.Caps
	NodeCancelled   func(chatID, nodeID string) bool
	// NodeCancelled/RepeatGuardTripped feed NewHooks; RepeatGuardTripped ends a node's round on the repeat
	// guard's hard stop, nil leaves only the soft refusal.
	RepeatGuardTripped func(chatID, nodeID, msg string) bool
	ExtTools           map[string]tool.Tool
	Memory             *memory.Store      // recall_memory (nil = not offered - see ResolveNames)
	MemoryRole         string             // recall_memory's role bucket; empty falls back to repo then user
	Ledger             ledger.LedgerStore // recall_memory's memory.recall ledger entries
	// RecordStore/NodeID/Coords back web_fetch's large-page storage and
	// grep_artifacts; nil RecordStore degrades both (no chat context).
	RecordStore *recordstore.Client
	NodeID      string
	Coords      *RoundCoords
	// Sink/TurnID: a node's chat SSE sink and turn id, which its tools' ctx lacks behind the A2A boundary.
	Sink   func(stream.SSEEvent)
	TurnID string
	// CallScope: the DAG node this build serves - fs, memory and guard scoping plus
	// extension sdk.CallInfo resolve from it, never from prompt text; zero outside a node.
	CallScope CallScope
}

type constructor func(Deps) (tool.Tool, error)

var registry = map[string]constructor{
	"web_search":     newWebSearch,
	"web_fetch":      newFetch,
	"summarize":      newSummarize,
	"current_date":   newCurrentDate,
	"stage_memory":   newStageMemory,
	"recall_memory":  newRecallMemory,
	"ask_user":       func(Deps) (tool.Tool, error) { return NewAskUserTool() },
	"read_file":      newReadFile,
	"list_dir":       newListDir,
	"glob":           newGlob,
	"grep":           newGrep,
	"render_ui":      newRenderUI,
	"grep_artifacts": newGrepArtifacts,
	"weather":        newWeather,
}

// BuiltinNames lists the registry's tool names, sorted; tools/promptlint measures each one's declaration.
func BuiltinNames() []string { return slices.Sorted(maps.Keys(registry)) }

// ResolveNames drops builtins whose dependency is off and reads load_memory, the tool's former name
// that deployed configs still list, as recall_memory, offered once. Artifact tools are per-dispatch.
func ResolveNames(configured []string, taskMemAvailable bool) (names []string) {
	names = make([]string, 0, len(configured))
	sawMemoryRecall := false
	for _, t := range configured {
		if IsNativeArtifactTool(t) {
			continue
		}
		switch t {
		case "stage_memory":
			if !taskMemAvailable {
				continue
			}
		case "recall_memory", "load_memory":
			if !taskMemAvailable || sawMemoryRecall {
				continue
			}
			sawMemoryRecall = true
			t = "recall_memory"
		}
		names = append(names, t)
	}
	return names
}

// ErrUnknownTool: a tools: entry no builtin or enabled extension provides.
var ErrUnknownTool = errors.New("unknown tool")

// Build constructs the named tools unwrapped; NewHooks supplies their per-call policy as agent callbacks.
func Build(names []string, d Deps) ([]tool.Tool, error) {
	if d.Client == nil {
		d.Client = &http.Client{Timeout: 30 * time.Second, Transport: httpx.NewTransport(nil)}
	}
	if d.Guarded == nil {
		d.Guarded = GuardedClient()
	}
	out := make([]tool.Tool, 0, len(names))
	for _, name := range names {
		t, err := buildOneTool(name, d)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func buildOneTool(name string, d Deps) (tool.Tool, error) {
	if ctor, ok := registry[name]; ok {
		t, err := ctor(d)
		if err != nil {
			return nil, fmt.Errorf("tools: build %q: %w", name, err)
		}
		return t, nil
	}
	et, ok := d.ExtTools[name]
	if !ok {
		return nil, fmt.Errorf("tools: unknown builtin tool %q: %w", name, ErrUnknownTool)
	}
	if et == nil {
		return nil, fmt.Errorf("tools: tool name %q is provided by more than one extension; use its <plugin>_%s prefixed form", name, name)
	}
	return withCallInfo(et, d)
}
