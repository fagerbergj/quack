package rest

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

// eventLog durably persists a chat's SSE run events to store.ChatEvent, backing
// the in-memory hub's replay across a server restart. The run loop assigns each
// event's per-chat seq (so the live tail and the durable log share one numbering)
// and hands rows here; a single goroutine drains them in order, off the run
// loop's hot path. A full queue drops-and-logs rather than wedging the run — the
// live hub still carried the event; only its durability is lost.
type eventLog struct {
	store *store.Store
	ch    chan store.ChatEvent
}

func newEventLog(s *store.Store) *eventLog {
	l := &eventLog{store: s, ch: make(chan store.ChatEvent, 4096)}
	go l.run()
	return l
}

func (l *eventLog) run() {
	for ce := range l.ch {
		if err := l.store.InsertChatEvent(context.Background(), ce); err != nil {
			slog.Warn("event log: persist failed; dropping", "component", "eventlog", "chat", ce.ChatID, "seq", ce.Seq, "err", err)
			continue
		}
		// Window a very long run to the durable replay ceiling, mirroring the hub's
		// bounded buffer. Rare (real runs are far smaller), so the trim is cheap.
		if ce.Seq > stream.MaxReplay {
			if err := l.store.TrimChatEvents(context.Background(), ce.ChatID, ce.Seq-stream.MaxReplay); err != nil {
				slog.Warn("event log: trim failed", "component", "eventlog", "chat", ce.ChatID, "err", err)
			}
		}
	}
}

// append enqueues an event row for persistence. Non-blocking: a backed-up queue
// drops the event from the durable log (the hub still delivered it live) rather
// than stalling the run loop.
func (l *eventLog) append(chatID string, seq int64, ev stream.SSEEvent) {
	js, err := marshalEvent(ev)
	if err != nil {
		slog.Warn("event log: marshal failed; dropping", "component", "eventlog", "chat", chatID, "seq", seq, "err", err)
		return
	}
	ce := store.ChatEvent{ChatID: chatID, Seq: seq, Event: js, CreatedAt: time.Now().UTC()}
	select {
	case l.ch <- ce:
	default:
		slog.Warn("event log: queue full; dropping event", "component", "eventlog", "chat", chatID, "seq", seq)
	}
}

// reset clears a chat's persisted events so a new run starts fresh at seq 1,
// mirroring the hub discarding the previous run's buffer on the first publish.
func (l *eventLog) reset(ctx context.Context, chatID string) {
	if err := l.store.DeleteChatEvents(ctx, chatID); err != nil {
		slog.Warn("event log: reset failed", "component", "eventlog", "chat", chatID, "err", err)
	}
}

// eventEnvelope is the persisted shape of a stream.SSEEvent: the event name plus
// its already-marshalled data, replayed verbatim on reconnect.
type eventEnvelope struct {
	Name string          `json:"name"`
	Data json.RawMessage `json:"data"`
}

func marshalEvent(ev stream.SSEEvent) (string, error) {
	data, err := json.Marshal(ev.Data)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(eventEnvelope{Name: ev.Name, Data: data})
	return string(b), err
}

// unmarshalEvent restores a persisted event. Data stays json.RawMessage so
// sseWriter re-marshals it to the identical bytes the live stream sent.
func unmarshalEvent(s string) (stream.SSEEvent, error) {
	var e eventEnvelope
	if err := json.Unmarshal([]byte(s), &e); err != nil {
		return stream.SSEEvent{}, err
	}
	return stream.SSEEvent{Name: e.Name, Data: e.Data}, nil
}
