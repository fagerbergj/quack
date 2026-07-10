package vetting

import (
	"regexp"
	"sync"
)

// Advisor-thread identity: how a worker's ask_advisor tool knows WHICH node's
// mentor conversation to continue.
//
// The tool's handler cannot derive the calling node from its own execution
// context: production workers are served over A2A (internal/agent.Serve →
// srv.Client()), so the handler runs inside the A2A SERVER's runner — a
// separate session (the A2A context session, AppName = the agent's name, a
// FRESH context per gate round) whose events carry no NodeInfo and whose
// state has none of the gate's keys. Branch is no help either: inside a tool
// call it is the worker's own run segment co-located ("worker@worker-r0", no
// node) and empty on the A2A server. The one channel that reliably crosses
// the A2A hop is the PROMPT — so the gate stamps a short marker line carrying
// a per-node token into every worker prompt, and the tool parses it back out
// of its UserContent. Deterministic and race-free: UserContent is fixed
// before the model ever runs.
//
// The token doubles as the key into a process-local registry carrying the
// node's task + acceptance rubric, which the tool uses to seed the mentor's
// session on its first consult. Process-local is sound here for the same
// reason the tool can hold the advisor agent at all: quack's A2A agents are
// co-located in one process (see internal/agent's package doc). If agents are
// ever promoted to standalone services, ask_advisor needs a rethink wholesale.

// advisorMarkerRe extracts the token from a marker line anywhere in the
// prompt. Plan/node IDs are slugs; anything up to the closing "]]" is the
// token.
var advisorMarkerRe = regexp.MustCompile(`\[\[quack:advisor-thread:([^\]]+)\]\]`)

// AdvisorThreadToken derives the stable per-node token: same across gate
// rounds (draft → revision), steered re-runs, and HITL pause/resume (all
// re-derive from the same plan+node), distinct across concurrent nodes and
// across plans.
func AdvisorThreadToken(planID, nodeID string) string {
	return planID + "/" + nodeID
}

// AdvisorThreadMarker renders the marker line the gate APPENDS to a worker
// prompt. Models treat the bracketed line as inert metadata. Trailing (not
// leading) placement pairs with ParseAdvisorThread's last-match rule: text
// that can carry a FOREIGN node's marker — an upstream output quoted into
// this prompt, or (over A2A) an earlier concurrent node's prompt event swept
// into this dispatch's message tail — always precedes this node's own
// trailing marker.
func AdvisorThreadMarker(token string) string {
	return "[[quack:advisor-thread:" + token + "]]"
}

// ParseAdvisorThread extracts the LAST advisor-thread token from prompt text
// (see AdvisorThreadMarker for why last); ok=false when no marker is present
// (e.g. the agent was invoked directly, outside any gated node).
func ParseAdvisorThread(text string) (token string, ok bool) {
	ms := advisorMarkerRe.FindAllStringSubmatch(text, -1)
	if len(ms) == 0 {
		return "", false
	}
	return ms[len(ms)-1][1], true
}

// AdvisorTask is what the mentor is told on first consult: the node's task
// and its acceptance rubric (the desired outcome).
type AdvisorTask struct {
	Task   string
	Rubric string
}

// advisorThreads is the process-local token → AdvisorTask registry. Written
// by the gated node (RegisterAdvisorThread before its worker runs, unregister
// when the node body exits — a HITL re-entry or retry re-registers), read by
// the ask_advisor tool on a thread's first consult.
var advisorThreads sync.Map

// RegisterAdvisorThread publishes a node's task+rubric under its token.
func RegisterAdvisorThread(token string, t AdvisorTask) {
	advisorThreads.Store(token, t)
}

// LookupAdvisorThread returns the registered task for token, if any.
func LookupAdvisorThread(token string) (AdvisorTask, bool) {
	v, ok := advisorThreads.Load(token)
	if !ok {
		return AdvisorTask{}, false
	}
	t, ok := v.(AdvisorTask)
	return t, ok
}

// UnregisterAdvisorThread removes a token's entry (bounds registry growth on
// a long-running server; the mentor's session itself persists regardless).
func UnregisterAdvisorThread(token string) {
	advisorThreads.Delete(token)
}
