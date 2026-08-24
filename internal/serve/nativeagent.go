package serve

import (
	"sync"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"

	"github.com/fagerbergj/quack/internal/agent"
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
	build func(nodeKey string, drain func() string) (adkagent.Agent, model.LLM, []tool.Tool, func(), error)
}

func (n nativeAgent) ForNode(nodeKey string, drain func() string) (adkagent.Agent, model.LLM, []tool.Tool, func(), error) {
	return n.build(nodeKey, drain)
}

// perNodeServers tracks currently-open per-node A2A servers (nativeAgent.
// ForNode) so process shutdown can close any whose owning node's release()
// never ran (an abandoned dynamic node, a crash mid-run) - self-pruning as
// each node releases normally, so a long-lived server's memory stays bounded
// by "nodes in flight right now", not "every node ever run".
type perNodeServers struct {
	mu   sync.Mutex
	open map[*agent.A2AServer]struct{}
}

func newPerNodeServers() *perNodeServers {
	return &perNodeServers{open: make(map[*agent.A2AServer]struct{})}
}

// track registers srv and returns its release func: idempotent (safe to
// call more than once; only the first call closes), untracks before
// closing so a concurrent closeAll sweep can never double-close.
func (p *perNodeServers) track(srv *agent.A2AServer) func() {
	p.mu.Lock()
	p.open[srv] = struct{}{}
	p.mu.Unlock()
	var once sync.Once
	return func() {
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
