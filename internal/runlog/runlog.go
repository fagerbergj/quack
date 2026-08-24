// Package runlog fans events to hub subscribers, durably persists them, and mirrors DAG state to the store.
package runlog

import (
	"context"
	"encoding/json"
	"iter"
	"log/slog"
	"time"

	"github.com/fagerbergj/quack/internal/dag"
	"github.com/fagerbergj/quack/internal/store"
	"github.com/fagerbergj/quack/internal/stream"
)

// EventLog persists SSE events to the store, draining in order off the hot path; full queue drops (durability loss only).
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
		// Trim to durable replay ceiling on long runs.
		if ce.Seq > stream.MaxReplay {
			if err := l.store.TrimChatEvents(context.Background(), ce.ChatID, ce.Seq-stream.MaxReplay); err != nil {
				slog.Warn("event log: trim failed", "component", "eventlog", "chat", ce.ChatID, "err", err)
			}
		}
	}
}

// Append enqueues an event row (non-blocking; drops if queue is full).
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

// Reset clears a chat's persisted events so a new run starts fresh at seq 1.
func (l *EventLog) Reset(ctx context.Context, chatID string) {
	if err := l.store.DeleteChatEvents(ctx, chatID); err != nil {
		slog.Warn("event log: reset failed", "component", "eventlog", "chat", chatID, "err", err)
	}
}

// eventEnvelope: persisted shape of stream.SSEEvent (name + marshalled data).
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

// UnmarshalEvent restores a persisted event (Data stays json.RawMessage for byte-for-byte fidelity).
func UnmarshalEvent(s string) (stream.SSEEvent, error) {
	var e eventEnvelope
	if err := json.Unmarshal([]byte(s), &e); err != nil {
		return stream.SSEEvent{}, err
	}
	return stream.SSEEvent{Name: e.Name, Data: e.Data}, nil
}

// Publisher fans one chat's events to hub + durable log under a monotonic seq (not safe for concurrent use).
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

// DriveResult is what draining one run's event stream to a Publisher
// determined - the plan/pause signals every dispatch path (REST, SDK
// extensions, the GitHub webhook) needs to decide what happens next, plus
// the orchestrator's own model/usage for StampTurn's tail.
type DriveResult struct {
	// SawPlan is true the moment any dag_plan event is seen, name-only -
	// even a test double's bare one with no Data payload.
	SawPlan bool
	// PlanID is "" unless a dag_plan event carried real DagPlanData; only
	// this gates SaveDagPlan/PersistNodeEvent (a name-only event has nothing
	// to persist).
	PlanID string
	// Paused is true iff a node_needs_input event carried real
	// NodeNeedsInputData (mirrors PlanID's Data-required rule).
	Paused     bool
	NeedsInput stream.NodeNeedsInputData
	// Model/Usage come from the top-level (NodeID == "") agent_complete -
	// the orchestrator's own reply. Empty for a DAG turn (PlanID != ""),
	// which credits tokens per-node on DagNode instead - see StampTurn.
	Model string
	Usage store.TurnUsage
}

// Step folds one event into res: DAG plan/node persistence, pause tracking,
// and model/usage capture - the per-event logic shared by Drive and
// rest.Handler's own loop (REST interleaves title-send between events, so it
// can't range through Drive directly; both must still agree on this step).
// persist gates the store writes (Drive passes pub != nil; a caller with no
// store, e.g. a test double, passes false and still gets plan/pause/usage
// tracking).
func (res *DriveResult) Step(st *store.Store, chatID, turnID string, persist bool, ev stream.SSEEvent) {
	if ev.Name == stream.EventDagPlan {
		res.SawPlan = true
		if d, ok := ev.Data.(stream.DagPlanData); ok {
			res.PlanID = d.PlanID
			if persist {
				SaveDagPlan(st, chatID, turnID, d)
			}
		}
	} else if res.PlanID != "" && persist {
		PersistNodeEvent(st, res.PlanID, ev)
	}
	if ev.Name == stream.EventNodeNeedsInput {
		if d, ok := ev.Data.(stream.NodeNeedsInputData); ok {
			res.Paused = true
			res.NeedsInput = d
		}
	}
	if ev.Name == stream.EventAgentComplete {
		if d, ok := ev.Data.(stream.AgentCompleteData); ok && d.NodeID == "" && d.Model != "" {
			res.Model = d.Model
			res.Usage = store.TurnUsage{
				PromptTokens: d.PromptTokens, CompletionTokens: d.CompletionTokens,
				ReasoningTokens: d.ReasoningTokens, TotalTokens: d.TotalTokens, CachedTokens: d.CachedTokens,
			}
		}
	}
}

// Drive drains run's event stream to pub, mirroring DAG plan/node state into
// st as it goes - the shared loop behind every dispatch path (REST, SDK
// extensions, the GitHub webhook), so the three don't each hand-roll their
// own copy. onErr, if non-nil, is called for each per-event error the
// iterator yields; the loop keeps draining regardless (never fatal). pub may
// be nil (a caller with no store to persist against, e.g. a test double) -
// persistence/publish are skipped, but plan/pause/usage tracking still runs.
func Drive(turnID string, st *store.Store, pub *Publisher, run iter.Seq2[stream.SSEEvent, error], onErr func(error)) DriveResult {
	var res DriveResult
	for ev, err := range run {
		if err != nil {
			if onErr != nil {
				onErr(err)
			}
			continue
		}
		chatID := ""
		if pub != nil {
			chatID = pub.chatID
		}
		res.Step(st, chatID, turnID, pub != nil, ev)
		if pub != nil {
			pub.Publish(ev)
		}
	}
	return res
}

// StampTurn stamps the orchestrator's model + token usage on the turn row -
// the tail every dispatch path (REST, SDK extensions) must share rather than
// duplicate (#831's lesson applied to model/usage stamping, not just the
// drain loop). A DAG turn (res.PlanID != "") is a no-op here: its tokens are
// already on DagNode, per node.
func StampTurn(ctx context.Context, st *store.Store, chatID, turnID string, res DriveResult) {
	if res.Model == "" || res.PlanID != "" {
		return
	}
	if err := st.SetTurnUsage(ctx, chatID, turnID, res.Model, res.Usage); err != nil {
		slog.Warn("runlog: stamp turn model/usage failed", "component", "runlog", "chat", chatID, "turn", turnID, "err", err)
	}
}

// PersistNodeEvent upserts DagNode state for node-lifecycle events; illegal
// transitions are logged, write proceeds. Synchronous on purpose: one
// goroutine per event gave no ordering, so a node_done write could be
// overwritten by an earlier event's later-scheduled goroutine, leaving a
// finished node stuck at running. Lifecycle events are a handful per node.
func PersistNodeEvent(st *store.Store, planID string, ev stream.SSEEvent) {
	t := time.Now().UTC()
	var nodeID string
	var to dag.NodeStatus
	n := store.DagNode{PlanID: planID}
	switch d := ev.Data.(type) {
	case stream.NodeQueuedData:
		nodeID, to = d.NodeID, dag.StatusQueued
		n.NodeID, n.Status, n.InstanceID = d.NodeID, string(to), st.InstanceID()
	case stream.NodeStartData:
		nodeID, to = d.NodeID, dag.StatusRunning
		n.NodeID, n.Status, n.StartedAt, n.InstanceID = d.NodeID, string(to), &t, st.InstanceID()
		n.TraceID = d.TraceID
	case stream.NodeDoneData:
		nodeID, to = d.NodeID, dag.StatusDone
		n.NodeID, n.Status, n.FinishedAt = d.NodeID, string(to), &t
		n.OutputPreview, n.Output = d.OutputPreview, d.Output
		n.Model, n.PromptTokens, n.CompletionTokens = d.Model, d.PromptTokens, d.CompletionTokens
		n.ReasoningTokens, n.TotalTokens, n.FinishReason = d.ReasoningTokens, d.TotalTokens, d.FinishReason
		n.CachedTokens = d.CachedTokens
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
}

// SaveDagPlan persists the plan row behind a dag_plan event.
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
