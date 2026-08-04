// Package runlog is the "run and persist" machinery shared by every driver of
// a chat run - the REST handler and the GitHub webhook dispatcher alike: fan
// each event to live hub subscribers, durably persist it (so a reconnecting
// client or a cold-restart replay sees the same history), and mirror DAG
// plan/node state into the store for the chat detail view (getChat).
package runlog

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

// EventLog durably persists a chat's SSE run events to store.ChatEvent, backing
// the in-memory hub's replay across a server restart. The run loop assigns each
// event's per-chat seq (so the live tail and the durable log share one
// numbering) and hands rows here; a single goroutine drains them in order, off
// the run loop's hot path. A full queue drops-and-logs rather than wedging the
// run - the live hub still carried the event; only its durability is lost.
type EventLog struct {
	store *store.Store
	ch    chan store.ChatEvent
}

func NewEventLog(s *store.Store) *EventLog {
	l := &EventLog{store: s, ch: make(chan store.ChatEvent, 4096)}
	go l.run()
	return l
}

func (l *EventLog) run() {
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

// Append enqueues an event row for persistence. Non-blocking: a backed-up queue
// drops the event from the durable log (the hub still delivered it live) rather
// than stalling the run loop.
func (l *EventLog) Append(chatID string, seq int64, ev stream.SSEEvent) {
	js, err := MarshalEvent(ev)
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

// Reset clears a chat's persisted events so a new run starts fresh at seq 1,
// mirroring the hub discarding the previous run's buffer on the first publish.
func (l *EventLog) Reset(ctx context.Context, chatID string) {
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

func MarshalEvent(ev stream.SSEEvent) (string, error) {
	data, err := json.Marshal(ev.Data)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(eventEnvelope{Name: ev.Name, Data: data})
	return string(b), err
}

// UnmarshalEvent restores a persisted event. Data stays json.RawMessage so the
// SSE writer re-marshals it to the identical bytes the live stream sent.
func UnmarshalEvent(s string) (stream.SSEEvent, error) {
	var e eventEnvelope
	if err := json.Unmarshal([]byte(s), &e); err != nil {
		return stream.SSEEvent{}, err
	}
	return stream.SSEEvent{Name: e.Name, Data: e.Data}, nil
}

// Publisher fans one chat run's events to live hub subscribers and the durable
// event log under a single monotonic per-chat seq. Not safe for concurrent use
// - a run has exactly one publisher, matching the "sole publisher" invariant
// that keeps seq monotonic without locking.
type Publisher struct {
	hub    *stream.Hub
	log    *EventLog
	chatID string
	seq    int64
}

func NewPublisher(hub *stream.Hub, log *EventLog, chatID string) *Publisher {
	return &Publisher{hub: hub, log: log, chatID: chatID}
}

func (p *Publisher) Publish(ev stream.SSEEvent) {
	p.seq++
	p.hub.Publish(p.chatID, p.seq, ev)
	p.log.Append(p.chatID, p.seq, ev)
}

// PersistNodeEvent upserts the persisted DagNode state for a node-lifecycle
// event (running / done / failed / needs_input / cancelled). Ignores non-node
// events. Every write routes through dag.CanTransition: an illegal transition
// from the node's current persisted status is a logged bug, not a silent
// write - the write proceeds regardless, since the SSE event is ground truth
// for what the executor actually did.
func PersistNodeEvent(st *store.Store, planID string, ev stream.SSEEvent) {
	t := time.Now().UTC()
	var nodeID string
	var to dag.NodeStatus
	n := store.DagNode{PlanID: planID}
	switch d := ev.Data.(type) {
	case stream.NodeQueuedData:
		// Persist the row at queue time so a reloaded chat (and `chat show`)
		// sees the node as queued rather than status-less until it starts.
		// InstanceID claims the node for this Store (see FailStaleDagNodes).
		nodeID, to = d.NodeID, dag.StatusQueued
		n.NodeID, n.Status, n.InstanceID = d.NodeID, string(to), st.InstanceID()
	case stream.NodeStartData:
		nodeID, to = d.NodeID, dag.StatusRunning
		n.NodeID, n.Status, n.StartedAt, n.InstanceID = d.NodeID, string(to), &t, st.InstanceID()
	case stream.NodeDoneData:
		nodeID, to = d.NodeID, dag.StatusDone
		n.NodeID, n.Status, n.FinishedAt = d.NodeID, string(to), &t
		n.OutputPreview, n.Output = d.OutputPreview, d.Output
		n.Model, n.PromptTokens, n.CompletionTokens = d.Model, d.PromptTokens, d.CompletionTokens
		n.ReasoningTokens, n.TotalTokens, n.FinishReason = d.ReasoningTokens, d.TotalTokens, d.FinishReason
		n.DurationMs, n.JudgeRounds = d.DurationMs, d.JudgeRounds
		n.JudgeFinalScore, n.JudgePassed = d.JudgeFinalScore, d.JudgePassed
	case stream.NodeFailedData:
		nodeID, to = d.NodeID, dag.StatusFailed
		n.NodeID, n.Status, n.Error, n.FinishedAt = d.NodeID, string(to), d.Error, &t
	case stream.NodeNeedsInputData:
		nodeID, to = d.NodeID, dag.StatusNeedsInput
		n.NodeID, n.Status = d.NodeID, string(to)
	case stream.NodePausedData:
		// No FinishedAt: a paused node is suspended, not finished - it stays
		// resumable (see dag.Executor.PauseNode).
		nodeID, to = d.NodeID, dag.StatusPaused
		n.NodeID, n.Status = d.NodeID, string(to)
	case stream.NodeCancelledData:
		nodeID, to = d.NodeID, dag.StatusCancelled
		n.NodeID, n.Status, n.FinishedAt = d.NodeID, string(to), &t
	default:
		return
	}
	go func() {
		ctx := context.Background()
		from := dag.StatusQueued
		if prev, err := st.GetDagNode(ctx, planID, nodeID); err == nil && prev != nil {
			from = dag.NodeStatus(prev.Status)
		}
		if !dag.CanTransition(from, to) {
			slog.Warn("persistNodeEvent: illegal node-status transition", "component", "dag",
				"plan_id", planID, "node_id", nodeID, "from", from, "to", to)
		}
		if err := st.UpsertDagNode(ctx, n); err != nil {
			slog.Warn("persistNodeEvent: upsert failed", "component", "dag",
				"plan_id", planID, "node_id", nodeID, "err", err)
		}
	}()
}

// SaveDagPlan persists the plan row behind a dag_plan event, off the run's hot
// path - the same JSON shape the REST handler stores.
func SaveDagPlan(st *store.Store, chatID, turnID string, d stream.DagPlanData) {
	planJSON, err := json.Marshal(d)
	if err != nil {
		slog.Warn("runlog: dag plan marshal failed", "component", "dag", "chat", chatID, "plan_id", d.PlanID, "err", err)
		return
	}
	go func() {
		if err := st.SaveDagPlan(context.Background(), chatID, d.PlanID, turnID, string(planJSON)); err != nil {
			slog.Warn("runlog: save dag plan failed", "component", "dag", "chat", chatID, "plan_id", d.PlanID, "err", err)
		}
	}()
}
