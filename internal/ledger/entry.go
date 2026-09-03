package ledger

import (
	"encoding/json"
	"time"
)

// Entry kinds for the typed, fail-closed AppendIntent path (V4 §4.8).
// node.* kinds are observational (best-effort at the call site); every
// other kind here is an intent (fail-closed at the call site).
const (
	KindArtifactRevision = "artifact.revision"
	KindJudgeRound       = "judge.round"
	KindDeliveryIntent   = "delivery.intent"
	KindDeliveryDone     = "delivery.done"
	KindNodeStarted      = "node.started"
	KindNodeDone         = "node.done"
	KindNodeFailed       = "node.failed"
)

// Entry is the WAL envelope (V4 §4.8): a state-changing intent appended
// before it is acted on, so the artifact store, node state and SSE stream
// are folds over these entries. Seq is allocated by the store on
// AppendIntent and ignored on input. Key (an artifact id+revision, or a
// delivery idempotency key) makes an intent idempotent on replay.
type Entry struct {
	Seq     int64           `json:"seq"`
	ChatID  string          `json:"chat_id"`
	TurnID  string          `json:"turn_id,omitempty"`
	NodeID  string          `json:"node_id,omitempty"`
	Kind    string          `json:"kind"`
	Key     string          `json:"key,omitempty"`
	At      time.Time       `json:"at"`
	Payload json.RawMessage `json:"payload,omitempty"`
}
