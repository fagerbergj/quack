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

// callFailure tracks consecutive generate() failures for one chat+node+agent
// triple. ADK's own runner swallows a worker node's returned error into a
// silent empty completion (no error reaches the session event) - this is the
// only place the real cause still exists once that happens (#1105).
type callFailure struct {
	err     error
	streak  int
	firstAt time.Time
	lastAt  time.Time
}

var (
	failuresMu sync.Mutex
	// ponytail: unbounded map keyed by chat+node+agent, swept by
	// RecordCallResult's success case, dag's LastFailure/consume reads, and
	// each RunGatedRefine/judge-round entry's own-role ClearFailure; add a
	// reaper if a leak ever shows up (bounded today by concurrently-running
	// invocations per chat+node+agent, not requests).
	failures = map[string]*callFailure{}
)

// failureKey includes agent (e.g. "judge" vs. the node's real agent name) so
// a judge failure after a worker's own success can't be mistaken for a
// worker gateway failure on a later, unrelated empty completion (PR #1109
// review finding 3) - judge coords always stamp Agent: "judge", distinct
// from any real node agent name.
func failureKey(chatID, node, agent string) string { return chatID + "\x00" + node + "\x00" + agent }

// RecordCallResult updates the consecutive-failure streak for chatID+node+agent.
// A nil err (success) clears the streak - a later empty completion for this
// node is then a real silent gap, not a masked gateway failure.
func RecordCallResult(chatID, node, agent string, err error) {
	if chatID == "" && node == "" {
		return
	}
	key := failureKey(chatID, node, agent)
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

// LastFailure reports the tracked consecutive-failure state for chatID+node+agent.
// ok is false when the last recorded call succeeded or nothing was recorded -
// callers must treat that as a genuine silent gap, not a masked error.
// duration is how long the streak has been running (lastAt - firstAt), not a
// point in time.
func LastFailure(chatID, node, agent string) (err error, streak int, duration time.Duration, ok bool) {
	key := failureKey(chatID, node, agent)
	failuresMu.Lock()
	defer failuresMu.Unlock()
	f := failures[key]
	if f == nil {
		return nil, 0, 0, false
	}
	return f.err, f.streak, f.lastAt.Sub(f.firstAt), true
}

// ClearFailure drops tracked state for chatID+node+agent, once a caller has
// consumed it into a durable report, the node succeeded on retry, or a fresh
// gate-refine invocation is starting (a node id is reused across turns/plans
// on the same chat, so a stale unconsumed record must not leak forward - #1109
// review finding 3).
func ClearFailure(chatID, node, agent string) {
	key := failureKey(chatID, node, agent)
	failuresMu.Lock()
	delete(failures, key)
	failuresMu.Unlock()
}

// toolRejectionsMu/toolRejections track the last `plan` tool rejection per
// chat (#1180): a planner turn whose plan calls were all rejected and that
// ends with no plan and no answer needs the same terminal-failure path as a
// gateway outage during planning, but the rejection text is quack's own
// dag.PlanRejectedError.Reason - never sanitized like a gateway error.
var (
	toolRejectionsMu sync.Mutex
	toolRejections   = map[string]string{}
)

// RecordPlanRejection notes the reason the plan judge declined a proposed
// plan on chatID. Overwrites any prior reason for the chat - only the most
// recent rejection matters to the give-up path.
func RecordPlanRejection(chatID, reason string) {
	if chatID == "" {
		return
	}
	toolRejectionsMu.Lock()
	defer toolRejectionsMu.Unlock()
	toolRejections[chatID] = reason
}

// LastPlanRejection returns the last recorded plan rejection reason for
// chatID, if any.
func LastPlanRejection(chatID string) (reason string, ok bool) {
	toolRejectionsMu.Lock()
	defer toolRejectionsMu.Unlock()
	reason, ok = toolRejections[chatID]
	return reason, ok
}

// ClearPlanRejection drops chatID's recorded rejection - called once a plan
// is accepted, so a later turn's genuine silent gap doesn't inherit a stale
// reason from an earlier, unrelated rejection on the same chat.
func ClearPlanRejection(chatID string) {
	toolRejectionsMu.Lock()
	delete(toolRejections, chatID)
	toolRejectionsMu.Unlock()
}

// storeFailuresMu/storeFailures track the last store (DB) error per chat
// (#1193): a dial/connection error surviving the pgdial retry gets swallowed
// into "no artifacts" by the planner's failSoftListArtifacts, so the run's
// terminal status needs the same give-up path a gateway outage uses, keyed
// separately from callFailure since a store error has no node/agent.
var (
	storeFailuresMu sync.Mutex
	storeFailures   = map[string]string{}
)

// SanitizeStoreError reduces a store/DB error to text safe to surface in a
// run outcome. A pgconn/gorm dial error's Error() text can itself contain
// the raw DSN in arbitrary quoting (#1200 review: regex-stripping credentials
// out of that text leaked fragments of quoted or @-containing passwords), so
// this never looks at err.Error() at all - only the structured dial address
// (net.OpError.Addr / net.DNSError.Name) and a coarse error class. The raw
// error is still available server-side via the slog.Warn call at the one
// caller (orchestrator.failSoftListArtifacts.List).
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

// dialErrorClass buckets a dial error into one of a few known classes
// without ever formatting err itself into the result - only errors.Is/As
// checks against structured error values, so nothing from err.Error() (which
// could echo a DSN) reaches the returned string.
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
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return "timeout"
	}
	return "connection error"
}

// RecordStoreFailure notes a sanitized store error for chatID, overwriting
// any prior one - only the most recent failure matters to the give-up path.
func RecordStoreFailure(chatID string, err error) {
	if chatID == "" || err == nil {
		return
	}
	storeFailuresMu.Lock()
	defer storeFailuresMu.Unlock()
	storeFailures[chatID] = SanitizeStoreError(err)
}

// ClearStoreFailure drops chatID's recorded store failure - called once a
// store call for that chat succeeds again, so a later genuine silent gap
// doesn't inherit a stale DB-outage reason.
func ClearStoreFailure(chatID string) {
	storeFailuresMu.Lock()
	delete(storeFailures, chatID)
	storeFailuresMu.Unlock()
}

// LastStoreFailure returns chatID's last recorded (already-sanitized) store
// failure reason, if any.
func LastStoreFailure(chatID string) (reason string, ok bool) {
	storeFailuresMu.Lock()
	defer storeFailuresMu.Unlock()
	reason, ok = storeFailures[chatID]
	return reason, ok
}

// statusRe pulls the HTTP status code out of openaimodel's "status <N>: ..."
// error shape without touching the rest of the string.
var statusRe = regexp.MustCompile(`status (\d{3})`)

// SanitizeGatewayError reduces a generate() error to a status-code
// classification safe to disclose publicly (a PR/issue comment) - unlike
// err.Error() itself, which for an HTTP failure carries the raw endpoint URL
// and the unmodified upstream response body (openaimodel.apiErr's shape), and
// for a 401 can echo the API key back verbatim (#1109 review finding 1). The
// raw error stays available server-side via slog (apiErr already logs it) and
// DagNode.Error is not touched by this - only the text handed to an
// extension's RunOutcome is. transient reports whether a retry is likely to
// help (5xx/408/429), for callers deciding whether "retry" is honest advice
// (finding 4).
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

// summaryStatusRe pulls the status code back out of a SanitizeGatewayError
// summary - used once that summary is all a later caller (e.g. the DagNode.Error
// column, read back after a round trip through the store) has left.
var summaryStatusRe = regexp.MustCompile(`returned (\d{3})`)

// TransientFromSummary reports whether a SanitizeGatewayError-shaped summary
// names a transient status class (429/408/5xx) - callers gate "retry" advice
// on this so it isn't shown for a 401/400/quota error that won't self-heal
// (#1109 review finding 4).
func TransientFromSummary(summary string) bool {
	m := summaryStatusRe.FindStringSubmatch(summary)
	if m == nil {
		return false
	}
	code, _ := strconv.Atoi(m[1])
	return code == http.StatusTooManyRequests || code == http.StatusRequestTimeout || code >= 500
}
