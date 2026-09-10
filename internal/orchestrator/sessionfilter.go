package orchestrator

import (
	"context"
	"iter"
	"strings"

	"google.golang.org/adk/v2/session"
)

// staleSessionRetries bounds AppendEvent's re-fetch-and-retry loop below.
const staleSessionRetries = 3

// conversationSessions filters Events() to only user/orchestrator events.
// Plan-graph/retry/resume runners read the raw session service directly.
type conversationSessions struct {
	session.Service
}

// isConversationEvent: user or orchestrator-authored (chat); all else is run internals.
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

// AppendEvent unwraps the view before delegating (underlying services type-assert their own session).
// A DAG plan's own nested runner.Run shares this chat's session id and can advance its UpdateTime
// mid-turn; ADK has no sentinel for the resulting stale write, only this substring - retry instead of failing the turn.
func (c conversationSessions) AppendEvent(ctx context.Context, sess session.Session, ev *session.Event) error {
	if v, ok := sess.(conversationSession); ok {
		sess = v.Session
	}
	err := c.Service.AppendEvent(ctx, sess, ev)
	for i := 0; err != nil && strings.Contains(err.Error(), "stale session error") && i < staleSessionRetries; i++ {
		resp, gerr := c.Service.Get(ctx, &session.GetRequest{AppName: sess.AppName(), UserID: sess.UserID(), SessionID: sess.ID()})
		if gerr != nil || resp == nil || resp.Session == nil {
			break
		}
		sess = resp.Session
		err = c.Service.AppendEvent(ctx, sess, ev)
	}
	return err
}

// conversationSession delegates to the wrapped session except Events() (filtered view).
type conversationSession struct {
	session.Session
}

func (s conversationSession) Events() session.Events {
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
