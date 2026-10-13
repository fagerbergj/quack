package extproc

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fagerbergj/quack/extensions/sdk"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolutils"
	"google.golang.org/genai"
)

// functionTool is the tool shape both ends handle: what quack's tool registry requires of an extension tool.
type functionTool interface {
	tool.Tool
	Declaration() *genai.FunctionDeclaration
	Run(ctx agent.Context, args any) (map[string]any, error)
}

// remoteTool is quack's handle on one of the child's tools: Run is a tool call into the current child.
type remoteTool struct {
	p    *Process
	spec toolSpec
	decl *genai.FunctionDeclaration
}

var _ functionTool = (*remoteTool)(nil)

func remoteTools(p *Process, specs []toolSpec) ([]tool.Tool, error) {
	out := make([]tool.Tool, 0, len(specs))
	for _, s := range specs {
		var d genai.FunctionDeclaration
		var raw struct {
			Params   json.RawMessage `json:"parametersJsonSchema"`
			Response json.RawMessage `json:"responseJsonSchema"`
		}
		if err := json.Unmarshal(s.Declaration, &d); err != nil {
			return nil, fmt.Errorf("tool %s: declaration: %w", s.Name, err)
		}
		_ = json.Unmarshal(s.Declaration, &raw) // the same bytes just decoded
		if raw.Params != nil {
			d.ParametersJsonSchema = raw.Params
		}
		if raw.Response != nil {
			d.ResponseJsonSchema = raw.Response
		}
		out = append(out, &remoteTool{p: p, spec: s, decl: &d})
	}
	return out, nil
}

func (t *remoteTool) Name() string                            { return t.spec.Name }
func (t *remoteTool) Description() string                     { return t.spec.Description }
func (t *remoteTool) IsLongRunning() bool                     { return t.spec.LongRunning }
func (t *remoteTool) Declaration() *genai.FunctionDeclaration { return t.decl }

func (t *remoteTool) ProcessRequest(_ agent.Context, req *model.LLMRequest) error {
	return toolutils.PackTool(req, t)
}

// Run carries the args, the function call id and the host's sdk.CallInfo; ctx ending cancels the child's call.
func (t *remoteTool) Run(ctx agent.Context, args any) (map[string]any, error) {
	m, ok := args.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("unexpected args type, got: %T", args)
	}
	params := toolParams{Name: t.spec.Name, CallID: ctx.FunctionCallID(), Args: m}
	if ci, ok := sdk.CallInfoFrom(ctx); ok {
		params.CallInfo = &ci
	}
	var res map[string]any
	if err := t.p.current().call(ctx, methodTool, params, &res); err != nil {
		return nil, err
	}
	return res, nil
}

// describeTools indexes the child's tools by name and reports them; quack refuses a tool it cannot run.
func describeTools(ts []tool.Tool) (map[string]functionTool, []toolSpec, error) {
	byName := make(map[string]functionTool, len(ts))
	specs := make([]toolSpec, 0, len(ts))
	for _, t := range ts {
		ft, ok := t.(functionTool)
		if !ok {
			return nil, nil, fmt.Errorf("tool %q is not a runnable function tool", t.Name())
		}
		decl, err := json.Marshal(ft.Declaration())
		if err != nil {
			return nil, nil, fmt.Errorf("tool %q: declaration: %w", t.Name(), err)
		}
		byName[t.Name()] = ft
		specs = append(specs, toolSpec{Name: t.Name(), Description: t.Description(), LongRunning: t.IsLongRunning(), Declaration: decl})
	}
	return byName, specs, nil
}

// toolInvocation is all of agent.Context a child's tool gets: the call's ctx with its cancel and CallInfo.
// Session, state and memory panic, which a functiontool returns as the call's error.
type toolInvocation struct{ agent.StrictContextMock }

func (*toolInvocation) Artifacts() agent.Artifacts { return nil }

func runTool(ctx context.Context, t functionTool, p toolParams) (map[string]any, error) {
	if p.CallInfo != nil {
		ctx = sdk.WithCallInfo(ctx, *p.CallInfo)
	}
	tc := agent.NewToolContext(&toolInvocation{agent.NewStrictContextMock(ctx)}, p.CallID, nil, nil)
	return t.Run(tc, p.Args)
}
