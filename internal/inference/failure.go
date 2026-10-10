package inference

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// keyed is a mutex-guarded map, the shape of each per-chat failure record below.
type keyed[V any] struct {
	mu sync.Mutex
	m  map[string]V
}

func newKeyed[V any]() *keyed[V] { return &keyed[V]{m: map[string]V{}} }

func (k *keyed[V]) get(key string) (V, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[key]
	return v, ok
}

// update stores fn(current, present) under key atomically.
func (k *keyed[V]) update(key string, fn func(V, bool) V) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.m[key]
	k.m[key] = fn(v, ok)
}

func (k *keyed[V]) put(key string, v V) { k.update(key, func(V, bool) V { return v }) }

func (k *keyed[V]) del(key string) {
	k.mu.Lock()
	delete(k.m, key)
	k.mu.Unlock()
}

// callFailure tracks consecutive generate() failures for one chat+node+agent. ADK's runner
// swallows a worker's error into a silent empty completion, so this is the only record of the cause.
type callFailure struct {
	err     error
	streak  int
	firstAt time.Time
	lastAt  time.Time
}

var (
	// ponytail: unbounded, swept by success, LastFailure consumers and ClearFailure; add a reaper if it ever leaks.
	failures       = newKeyed[callFailure]()
	planRejections = newKeyed[string]()
	storeFailures  = newKeyed[string]()
)

// failureKey includes agent so a judge failure (Agent "judge") is never mistaken for the worker's.
func failureKey(chatID, node, agent string) string { return chatID + "\x00" + node + "\x00" + agent }

// RecordCallResult updates the chat+node+agent failure streak; a nil err clears it.
func RecordCallResult(chatID, node, agent string, err error) {
	if chatID == "" && node == "" {
		return
	}
	key := failureKey(chatID, node, agent)
	if err == nil {
		failures.del(key)
		return
	}
	failures.update(key, func(f callFailure, ok bool) callFailure {
		if !ok {
			f.firstAt = time.Now()
		}
		f.err = err
		f.streak++
		f.lastAt = time.Now()
		return f
	})
}

// LastFailure reports the open failure streak; !ok means a genuine silent gap, not a masked error.
// duration is lastAt - firstAt.
func LastFailure(chatID, node, agent string) (err error, streak int, duration time.Duration, ok bool) {
	f, ok := failures.get(failureKey(chatID, node, agent))
	if !ok {
		return nil, 0, 0, false
	}
	return f.err, f.streak, f.lastAt.Sub(f.firstAt), true
}

// ClearFailure drops the record once consumed or before a fresh gate-refine run:
// node ids are reused across turns, so a stale record must not leak forward.
func ClearFailure(chatID, node, agent string) { failures.del(failureKey(chatID, node, agent)) }

// RecordPlanRejection keeps the latest plan-judge rejection reason for chatID. A planner turn
// with every execute rejected needs the gateway-outage give-up path, with quack's own (unsanitized) text.
func RecordPlanRejection(chatID, reason string) {
	if chatID != "" {
		planRejections.put(chatID, reason)
	}
}

func LastPlanRejection(chatID string) (reason string, ok bool) { return planRejections.get(chatID) }

// ClearPlanRejection runs once a plan is accepted, so a later silent gap doesn't inherit a stale reason.
func ClearPlanRejection(chatID string) { planRejections.del(chatID) }

// SanitizeStoreError reduces a store error to text safe for a run outcome. It never reads err.Error():
// a pgconn dial error can embed the raw DSN, so only the dial address and an error class are used.
func SanitizeStoreError(err error) string {
	if err == nil {
		return ""
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		class := dialErrorClass(opErr.Err)
		if opErr.Addr != nil {
			return fmt.Sprintf("database unavailable: dial %s: %s", opErr.Addr, class)
		}
		return fmt.Sprintf("database unavailable: %s", class)
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return fmt.Sprintf("database unavailable: dial %s: no such host", dnsErr.Name)
	}
	return "database unavailable: connection error"
}

// dialErrorClass buckets err with errors.Is/As only, so no err.Error() text (a possible DSN) leaks.
func dialErrorClass(err error) string {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ETIMEDOUT):
		return "timeout"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "no such host"
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	return "connection error"
}

// IsDialFailure reports a connection-level failure (refused, timeout, DNS), not an HTTP status.
func IsDialFailure(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ETIMEDOUT) {
		return true
	}
	var dnsErr *net.DNSError
	var opErr *net.OpError
	return errors.As(err, &dnsErr) || errors.As(err, &opErr)
}

// RecordStoreFailure keeps chatID's latest sanitized store error: the planner swallows a DB
// outage into "no artifacts", so the run needs the same give-up path a gateway outage uses.
func RecordStoreFailure(chatID string, err error) {
	if chatID != "" && err != nil {
		storeFailures.put(chatID, SanitizeStoreError(err))
	}
}

// ClearStoreFailure runs once a store call succeeds again, so a later silent gap isn't blamed on the DB.
func ClearStoreFailure(chatID string) { storeFailures.del(chatID) }

func LastStoreFailure(chatID string) (reason string, ok bool) { return storeFailures.get(chatID) }

var statusRe = regexp.MustCompile(`status (\d{3})`)

// SanitizeGatewayError reduces a generate() error to a status class safe to post publicly: err.Error()
// carries the endpoint URL and raw body, and a 401 can echo the API key. transient: 5xx/408/429.
func SanitizeGatewayError(err error) (summary string, transient bool) {
	if err == nil {
		return "", false
	}
	m := statusRe.FindStringSubmatch(err.Error())
	if m == nil {
		return "model call failed (non-HTTP error)", false
	}
	code, _ := strconv.Atoi(m[1])
	transient = code == http.StatusTooManyRequests || code == http.StatusRequestTimeout || code >= 500
	if text := http.StatusText(code); text != "" {
		return fmt.Sprintf("model gateway returned %d %s", code, text), transient
	}
	return fmt.Sprintf("model gateway returned status %d", code), transient
}
