package tools

// Shared test doubles for agent.Context (originally beside the cd tool's
// tests; the consumers — repeatguard/nodescope/namespace/setup tests — remain).

import (
	"context"
	"iter"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// fakeState is an in-memory session.State for exercising the cwd round-trip.
type fakeState struct{ m map[string]any }

func (s *fakeState) Get(k string) (any, error) {
	if v, ok := s.m[k]; ok {
		return v, nil
	}
	return nil, session.ErrStateKeyNotExist
}
func (s *fakeState) Set(k string, v any) error { s.m[k] = v; return nil }
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

func (c *fakeCtx) UserContent() *genai.Content          { return nil }
func (c *fakeCtx) InvocationID() string                 { return "inv" }
func (c *fakeCtx) AgentName() string                    { return "test" }
func (c *fakeCtx) ReadonlyState() session.ReadonlyState { return c.state }
func (c *fakeCtx) UserID() string                       { return "u" }
func (c *fakeCtx) AppName() string                      { return "app" }
func (c *fakeCtx) SessionID() string                    { return "sess" }
func (c *fakeCtx) Branch() string                       { return "" }
func (c *fakeCtx) Artifacts() adkagent.Artifacts        { return nil }
func (c *fakeCtx) State() session.State                 { return c.state }
