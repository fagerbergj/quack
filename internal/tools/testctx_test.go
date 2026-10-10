package tools

// Shared test doubles for agent.Context (originally beside the cd tool's
// tests; the consumers - repeatguard/nodescope/namespace/setup tests - remain).

import (
	"context"
	"iter"
	"sync"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
)

// fakeState is an in-memory session.State for exercising the cwd round-trip.
type fakeState struct {
	m      map[string]any
	setErr error // when set, Set fails - for the execute persist-plan error path
}

func (s *fakeState) Get(k string) (any, error) {
	if v, ok := s.m[k]; ok {
		return v, nil
	}
	return nil, session.ErrStateKeyNotExist
}
func (s *fakeState) Set(k string, v any) error {
	if s.setErr != nil {
		return s.setErr
	}
	s.m[k] = v
	return nil
}
func (s *fakeState) All() iter.Seq2[string, any] {
	return func(yield func(string, any) bool) {
		for k, v := range s.m {
			if !yield(k, v) {
				return
			}
		}
	}
}

// fakeCtx embeds StrictContextMock and serves a real State so cwd persists
// across calls (mirrors internal/agent/compaction_test.go's fakeCtx).
type fakeCtx struct {
	adkagent.StrictContextMock
	state *fakeState
}

func newFakeCtx() *fakeCtx {
	return &fakeCtx{
		StrictContextMock: adkagent.StrictContextMock{Ctx: context.Background()},
		state:             &fakeState{m: map[string]any{}},
	}
}

func (c *fakeCtx) UserContent() *genai.Content                          { return nil }
func (c *fakeCtx) InvocationID() string                                 { return "inv" }
func (c *fakeCtx) AgentName() string                                    { return "test" }
func (c *fakeCtx) ReadonlyState() session.ReadonlyState                 { return c.state }
func (c *fakeCtx) UserID() string                                       { return "u" }
func (c *fakeCtx) AppName() string                                      { return "app" }
func (c *fakeCtx) SessionID() string                                    { return "sess" }
func (c *fakeCtx) Session() session.Session                             { return nil }
func (c *fakeCtx) ToolConfirmation() *toolconfirmation.ToolConfirmation { return nil }
func (c *fakeCtx) Branch() string                                       { return "" }
func (c *fakeCtx) Artifacts() adkagent.Artifacts                        { return nil }
func (c *fakeCtx) State() session.State                                 { return c.state }

// fakeRunnable records executions without the agent.Context plumbing a functiontool needs.
type fakeRunnable struct {
	mu   sync.Mutex
	runs int
}

func (*fakeRunnable) Name() string        { return "risky_op" }
func (*fakeRunnable) Description() string { return "a risky operation" }
func (*fakeRunnable) IsLongRunning() bool { return false }
func (*fakeRunnable) Declaration() *genai.FunctionDeclaration {
	return &genai.FunctionDeclaration{Name: "risky_op"}
}
func (*fakeRunnable) ProcessRequest(adkagent.Context, *model.LLMRequest) error { return nil }
func (f *fakeRunnable) Run(adkagent.Context, any) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs++
	return map[string]any{"ok": true}, nil
}
func (f *fakeRunnable) runCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runs
}

// hooked runs a tool the way ADK's flow does with h wired: Before callbacks, Run, OnToolError, After.
type hooked struct {
	runnableTool
	h *Hooks
}

func hook(h *Hooks, p HookPolicy, t tool.Tool) hooked {
	h.Set(p, t)
	return hooked{runnableTool: t.(runnableTool), h: h}
}

func (w hooked) Run(ctx adkagent.Context, args any) (map[string]any, error) {
	m, _ := args.(map[string]any)
	var cfg llmagent.Config
	w.h.Wire(&cfg)
	var res map[string]any
	var err error
	for _, cb := range cfg.BeforeToolCallbacks {
		if res, err = cb(ctx, w, m); res != nil || err != nil {
			break
		}
	}
	if res == nil && err == nil {
		res, err = w.runnableTool.Run(ctx, args)
	}
	if err != nil {
		res = nil
		for _, cb := range cfg.OnToolErrorCallbacks {
			if r, e := cb(ctx, w, m, err); r != nil || e != nil {
				res, err = r, e
				break
			}
		}
	}
	for _, cb := range cfg.AfterToolCallbacks {
		if r, e := cb(ctx, w, m, res, err); r != nil || e != nil {
			return r, e
		}
	}
	return res, err
}
