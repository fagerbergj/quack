package serve

import (
	"sync"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"

	"github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/stream"
)

// nativeAgent is buildAgents' clientMap entry for a native (co-located,
// non-ACP) configured agent (#609). It embeds a prototype instance - built
// once, at startup, NEVER Run - purely so Name()/Description() work for the
// planner roster; every actual dispatch goes through ForNode, which builds a
// worker exclusive to one DAG node (fresh model, fresh tools, its own
// loopback A2A server) so two nodes sharing this configured agent
// concurrently never race SetLedgerCoords/ledger.StampCoords's shared
// mutable coordinate field.
type nativeAgent struct {
	adkagent.Agent
	build func(nodeKey string, drain func() string, artifacts artifact.Service, appName, userID, chatID, nodeID string, sink func(stream.SSEEvent)) (adkagent.Agent, model.LLM, []tool.Tool, func(round int, turnID, headSHA, triggerAnnotation string), func(paused bool), error)
}

// ForNode builds this node's list/read/edit/write_<kind> artifact tools
// (internal/tools.BuildNativeArtifactTools) into the worker's builtins
// before construction - the same mechanism check_mermaid/format-markdown
// tools go through (buildWorker's builtins), not a parallel one (#1123).
// sink is grabbed from the caller's ctx at build time (stream.YieldFromContext)
// and closed over by this node's own A2A server - it can't cross the A2A
// wire later, so agent.Serve needs it passed in explicitly (see its doc).
func (n nativeAgent) ForNode(nodeKey string, drain func() string, artifacts artifact.Service, appName, userID, chatID, nodeID string, sink func(stream.SSEEvent)) (adkagent.Agent, model.LLM, []tool.Tool, func(round int, turnID, headSHA, triggerAnnotation string), func(paused bool), error) {
	return n.build(nodeKey, drain, artifacts, appName, userID, chatID, nodeID, sink)
}

// perNodeServers tracks currently-open per-node A2A servers (nativeAgent.ForNode) so process
// shutdown can close any whose owning node's release() never ran (an abandoned dynamic node, a
// crash mid-run) - self-pruning as each node releases, so memory stays bounded by nodes in flight.
type perNodeServers struct {
	mu   sync.Mutex
	open map[*agent.A2AServer]struct{}
}

func newPerNodeServers() *perNodeServers {
	return &perNodeServers{open: make(map[*agent.A2AServer]struct{})}
}

// track registers srv and returns its release func: idempotent (safe to
// call more than once; only the first call's argument and effects apply),
// untracks before closing so a concurrent closeAll sweep can never double-close.
//
// release only closes this dispatch's own per-node A2A server. The
// deterministic worker session (agent.scopeMessage) it served now outlives
// every dispatch, paused or not - a node's later reuse (a brand new ForNode
// call to the SAME session id) must always find its prior history, so the
// only place that session gets reaped is chat archive/delete
// (store.Store.ReapNodeSessions), not a single node's completion.
func (p *perNodeServers) track(srv *agent.A2AServer) func(paused bool) {
	p.mu.Lock()
	p.open[srv] = struct{}{}
	p.mu.Unlock()
	var once sync.Once
	return func(paused bool) {
		once.Do(func() {
			p.mu.Lock()
			delete(p.open, srv)
			p.mu.Unlock()
			_ = srv.Close()
		})
	}
}

// closeAll closes every still-open per-node server - the shutdown backstop
// for whichever nodes' own release() never ran.
func (p *perNodeServers) closeAll() {
	p.mu.Lock()
	open := make([]*agent.A2AServer, 0, len(p.open))
	for srv := range p.open {
		open = append(open, srv)
	}
	p.mu.Unlock()
	for _, srv := range open {
		_ = srv.Close()
	}
}
