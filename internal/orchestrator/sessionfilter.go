package orchestrator

import (
	"context"
	"iter"

	"google.golang.org/adk/v2/session"
)

// conversationSessions is the session view the orchestrator's OWN llmagent
// runner sees: Events() yields only user- and orchestrator-authored events.
// The plan graph runs in the SAME session (sessionID == chatID) and writes
// worker drafts, gate prompts, and relay activity there too; ADK builds the
// orchestrator's request straight from session history and, because its
// branch filter passes anything on the orchestrator's own branch ("") plus
// any branchless event, would otherwise convert all of that run-internal
// traffic into "for context" text on every downstream turn.
//
// Only the phase-1 llmagent runner uses this view - plan-graph/retry/resume
// runners read the raw session service; they need the internals.
type conversationSessions struct {
	session.Service
}

// isConversationEvent is the one predicate: the same author discipline as
// store.groupSessionEvents (user turns + orchestrator output are the chat;
// worker/gate/relay authors are run internals).
func isConversationEvent(ev *session.Event) bool {
	return ev != nil && (ev.Author == "user" || ev.Author == orchestratorName)
}

func (c conversationSessions) Create(ctx context.Context, req *session.CreateRequest) (*session.CreateResponse, error) {
	resp, err := c.Service.Create(ctx, req)
	if err != nil || resp == nil || resp.Session == nil {
		return resp, err
	}
	return &session.CreateResponse{Session: conversationSession{resp.Session}}, nil
}

func (c conversationSessions) Get(ctx context.Context, req *session.GetRequest) (*session.GetResponse, error) {
	resp, err := c.Service.Get(ctx, req)
	if err != nil || resp == nil || resp.Session == nil {
		return resp, err
	}
	return &session.GetResponse{Session: conversationSession{resp.Session}}, nil
}

// AppendEvent unwraps the view before delegating: the underlying services
// type-assert their own concrete session (e.g. inMemoryService.AppendEvent's
// curSession.(*session)), so they must receive the session they created -
// and every append (the user's message, the orchestrator's replies and tool
// events) is persisted unfiltered; only the READ view is dieted.
func (c conversationSessions) AppendEvent(ctx context.Context, sess session.Session, ev *session.Event) error {
	if v, ok := sess.(conversationSession); ok {
		sess = v.Session
	}
	return c.Service.AppendEvent(ctx, sess, ev)
}

// conversationSession delegates everything to the wrapped session except
// Events(), which yields the filtered view. State, ID, and the rest pass
// through untouched (the execute tool's ExecPlanKey state, for one, must
// keep working).
type conversationSession struct {
	session.Session
}

func (s conversationSession) Events() session.Events {
	// Materialize per call: the underlying session grows during a turn (the
	// runner appends the user message and each model/tool event), and the
	// contents builder re-reads Events() before every model call - a fresh
	// filtered snapshot each time keeps the in-turn view current.
	var filtered []*session.Event
	for ev := range s.Session.Events().All() {
		if isConversationEvent(ev) {
			filtered = append(filtered, ev)
		}
	}
	return conversationEvents(filtered)
}

// conversationEvents is a filtered event list satisfying session.Events.
type conversationEvents []*session.Event

func (e conversationEvents) All() iter.Seq[*session.Event] {
	return func(yield func(*session.Event) bool) {
		for _, ev := range e {
			if !yield(ev) {
				return
			}
		}
	}
}

func (e conversationEvents) Len() int                { return len(e) }
func (e conversationEvents) At(i int) *session.Event { return e[i] }
