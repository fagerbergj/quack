package orchestrator

import (
	"context"
	"iter"

	"google.golang.org/adk/v2/session"
)

// conversationSessions is the session view the orchestrator's OWN llmagent
// runner sees: every Get/Create returns a session whose Events() yields only
// CONVERSATION events — user-authored turns and the orchestrator's own events
// (its replies, its plan/execute/get_user_choice calls and their results, and
// the persistAnswer-delivered plan answers). Everything the plan graph writes
// into the same session (worker drafts and tool traffic, gate prompt-delivery
// events, plan-wrapper structural events, A2A-relayed activity) is invisible
// to the orchestrator's request builder.
//
// Why this exists (live failure 2026-07-11): the orchestrator is pinned
// Mode: ModeChat (the amnesia fix), so ADK builds its request from session
// history — and the plan graph runs in the SAME session (sessionID ==
// chatID). ADK's contents builder cannot exclude the run internals for us:
//   - its branch filter (adk/v2 internal/llminternal/contents_processor.go:92
//   - eventBelongsToBranch at :205-216) passes EVERY event when the
//     requesting invocation's branch is "" — which the orchestrator's is —
//     and also passes any BRANCHLESS event (session.NewEvent stamps no
//     branch: session/session.go:226-233), which covers the gate's
//     emitPrompt events among others;
//   - events by OTHER authors are not dropped but CONVERTED
//     (contents_processor.go:105-106 → ConvertForeignEvent at :558-593) into
//     user-role "For context: [author] said/called/returned …" text — so one
//     heavy coding run (file reads, command output, revise rounds) became
//     ~110K tokens of foreign-event text in the next turn's request.
//
// This wrapper applies, at the source, the same author discipline
// store.groupSessionEvents already uses for chat persistence: user +
// orchestrator authors ARE the conversation; nothing else is. The delivered
// answer survives because Run/resumeNodeRun/RetryNode persist it as an
// orchestrator-authored event (persistAnswer) — the conversational record is
// complete without any run-internal event.
//
// Scope: ONLY the phase-1 llmagent runner uses this view. The plan-graph,
// retry, and resume runners — and every direct o.sessions read (PriorEvents,
// pending-interrupt scans, the REST status handler) — stay on the raw
// service: they legitimately need the run internals.
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
// curSession.(*session)), so they must receive the session they created —
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
	// contents builder re-reads Events() before every model call — a fresh
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
