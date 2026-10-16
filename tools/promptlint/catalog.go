package main

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"maps"
	"os"
	"slices"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/skilltoolset"
	"google.golang.org/adk/v2/tool/skilltoolset/skill"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/extensions/github"
	"github.com/fagerbergj/quack/extensions/sdk"
	"github.com/fagerbergj/quack/internal/tools"
	"github.com/fagerbergj/quack/internal/workspace"

	_ "github.com/fagerbergj/quack/extensions/noop"
	_ "github.com/fagerbergj/quack/extensions/remarkable"
	_ "github.com/fagerbergj/quack/extensions/sleeper"
	_ "github.com/fagerbergj/quack/extensions/usage"
)

// toolDef is what a model is sent for one tool: name, description and the parameter schema.
type toolDef struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Parameters  any    `json:"parameters,omitempty"`
	Provider    string `json:"provider"`
}

// skillToolset is the provider name for load_skill and friends, offered to any agent with skills:.
const skillToolset = "skilltoolset"

// extConfigs is the least config each extension's side-effect-free factory accepts; extStubs covers
// github, whose factory parses a real private key.
var (
	extConfigs = map[string]string{
		"remarkable": "base_url: http://stub\nemail: a\npassword: b\n",
		"usage":      "prometheus_url: http://stub\n",
	}
	extStubs = map[string]func() []tool.Tool{
		"github": func() []tool.Tool { return (&github.App{}).Tools() },
	}
)

// catalog builds every tool an agent can be offered, with stub deps, and returns the declarations
// openaimodel sends (name, description, parameters). An extension it cannot build is an error.
func catalog() ([]toolDef, error) {
	tmp, err := os.MkdirTemp("", "promptlint-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()
	jail, err := workspace.NewJail(tmp)
	if err != nil {
		return nil, err
	}
	built, err := tools.Build(tools.BuiltinNames(), tools.Deps{
		WebSearch: tools.Backend{Kind: "searxng", URL: "http://stub"}, Summarizer: stubLLM{},
		Workspace: jail, WorkspaceUserID: "promptlint",
	})
	if err != nil {
		return nil, err
	}
	defs := decls("builtin", built)
	core, err := coreTools()
	if err != nil {
		return nil, err
	}
	defs = append(defs, decls("core", core)...)
	skillTS, err := skilltoolset.New(context.Background(), skilltoolset.Config{Source: skill.NewFileSystemSource(os.DirFS(tmp))})
	if err != nil {
		return nil, err
	}
	sts, _ := skillTS.Tools(nil)
	defs = append(defs, decls(skillToolset, sts)...)
	ext, err := extensionTools(tmp)
	return append(defs, ext...), err
}

// coreTools are the per-dispatch and orchestrator tools that the registry does not hold.
func coreTools() ([]tool.Tool, error) {
	out, err := tools.BuildNativeArtifactTools(nil, "", nil, "", "")
	if err != nil {
		return nil, err
	}
	ctors := []func() (tool.Tool, error){
		func() (tool.Tool, error) { return tools.NewListNodesTool(nil, nil) },
		func() (tool.Tool, error) { return tools.NewCreatePlanTool(nil, "", nil, nil, nil, nil, nil) },
		func() (tool.Tool, error) { return tools.NewEditPlanTool(nil, "", nil, nil, nil, nil, nil) },
		func() (tool.Tool, error) {
			return tools.NewExecuteTool(nil, nil, nil, nil, nil, nil, nil, "", nil, nil, nil, "", nil, false, "", nil)
		},
		tools.NewGetUserChoiceTool,
		func() (tool.Tool, error) { return tools.NewCommitMemoryTool(nil, "", "", "") },
	}
	for _, c := range ctors {
		t, err := c()
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func extensionTools(dataDir string) ([]toolDef, error) {
	var defs []toolDef
	regs := sdk.Registered()
	for _, name := range slices.Sorted(maps.Keys(regs)) {
		if stub, ok := extStubs[name]; ok {
			defs = append(defs, decls(name, stub())...)
			continue
		}
		ext, err := regs[name](sdk.Host{DataDir: dataDir}, []byte(extConfigs[name]))
		if err != nil {
			return nil, fmt.Errorf("extension %s: build with no config (add an extConfigs entry): %w", name, err)
		}
		defs = append(defs, decls(name, ext.Tools())...)
	}
	return defs, nil
}

type declarer interface {
	Declaration() *genai.FunctionDeclaration
}

func decls(provider string, ts []tool.Tool) []toolDef {
	out := make([]toolDef, 0, len(ts))
	for _, t := range ts {
		d := toolDef{Name: t.Name(), Description: t.Description(), Provider: provider}
		if dd, ok := t.(declarer); ok && dd.Declaration() != nil {
			fd := dd.Declaration()
			d.Description = fd.Description
			d.Parameters = fd.ParametersJsonSchema
			if d.Parameters == nil && fd.Parameters != nil {
				d.Parameters = fd.Parameters
			}
		}
		out = append(out, d)
	}
	return out
}

// size is the bytes of the JSON a provider receives for this tool.
func (d toolDef) size() int {
	b, _ := json.Marshal(struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Parameters  any    `json:"parameters,omitempty"`
	}{d.Name, d.Description, d.Parameters})
	return len(b)
}

type stubLLM struct{}

func (stubLLM) Name() string { return "promptlint-stub" }
func (stubLLM) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(func(*model.LLMResponse, error) bool) {}
}
