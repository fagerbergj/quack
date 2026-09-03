package inference

import (
	"sync"
	"time"
)

// callFailure tracks consecutive generate() failures for one chat+node pair.
// ADK's own runner swallows a worker node's returned error into a silent
// empty completion (no error reaches the session event) - this is the only
// place the real cause still exists once that happens (#1105).
type callFailure struct {
	err     error
	streak  int
	firstAt time.Time
	lastAt  time.Time
}

var (
	failuresMu sync.Mutex
	// ponytail: unbounded map keyed by chat+node, swept only by RecordCallResult's
	// success case and dag's LastFailure/consume reads; add a reaper if a
	// leak ever shows up (bounded today by concurrently-running nodes, not requests).
	failures = map[string]*callFailure{}
)

func failureKey(chatID, node string) string { return chatID + "\x00" + node }

// RecordCallResult updates the consecutive-failure streak for chatID+node. A
// nil err (success) clears the streak - a later empty completion for this
// node is then a real silent gap, not a masked gateway failure.
func RecordCallResult(chatID, node string, err error) {
	if chatID == "" && node == "" {
		return
	}
	key := failureKey(chatID, node)
	failuresMu.Lock()
	defer failuresMu.Unlock()
	if err == nil {
		delete(failures, key)
		return
	}
	f := failures[key]
	if f == nil {
		f = &callFailure{firstAt: time.Now()}
		failures[key] = f
	}
	f.err = err
	f.streak++
	f.lastAt = time.Now()
}

// LastFailure reports the tracked consecutive-failure state for chatID+node.
// ok is false when the last recorded call succeeded or nothing was recorded -
// callers must treat that as a genuine silent gap, not a masked error.
func LastFailure(chatID, node string) (err error, streak int, since time.Duration, ok bool) {
	key := failureKey(chatID, node)
	failuresMu.Lock()
	defer failuresMu.Unlock()
	f := failures[key]
	if f == nil {
		return nil, 0, 0, false
	}
	return f.err, f.streak, f.lastAt.Sub(f.firstAt), true
}

// ClearFailure drops tracked state for chatID+node, once a caller has
// consumed it into a durable report (or the node succeeded on retry).
func ClearFailure(chatID, node string) {
	key := failureKey(chatID, node)
	failuresMu.Lock()
	delete(failures, key)
	failuresMu.Unlock()
}
