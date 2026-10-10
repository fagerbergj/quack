package serve

import (
	"context"
	"sync"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"

	"github.com/fagerbergj/quack/internal/agent"
	"github.com/fagerbergj/quack/internal/artifactsrc"
	"github.com/fagerbergj/quack/internal/stream"
)

// nativeAgent's prototype is never Run; it only serves Name()/Description() for the planner roster.
// ForNode builds a per-node worker so concurrent nodes never race the shared ledger coordinate field.
type nativeAgent struct {
	adkagent.Agent
	build nodeBuilder
}

// roundCoordsSetter stamps a node's per-round ledger coordinates.
type roundCoordsSetter func(round int, turnID, headSHA, triggerAnnotation string)

// nodeRelease releases the node's pinned session, recording whether it stays paused.
type nodeRelease func(paused bool)

// promptRefresher re-resolves a node's system prompt per round; one per dispatch, so nodes of the same
// agent never move each other's prompt.
type promptRefresher func(ctx context.Context) artifactsrc.Artifact

// nodeBuilder builds one native node's dispatch worker.
type nodeBuilder func(ctx context.Context, nodeKey, advisorToken string, drain func() string, artifacts artifact.Service, appName, userID, chatID, nodeID string, sink func(stream.SSEEvent)) (adkagent.Agent, model.LLM, []tool.Tool, roundCoordsSetter, promptRefresher, nodeRelease, error)

// ForNode builds one node's worker and tools; sink and ctx's turn id go to the tools, whose A2A-served
// ctx carries neither. release(paused) closes its A2A server.
func (n nativeAgent) ForNode(ctx context.Context, nodeKey, advisorToken string, drain func() string, artifacts artifact.Service, appName, userID, chatID, nodeID string, sink func(stream.SSEEvent)) (adkagent.Agent, model.LLM, []tool.Tool, func(round int, turnID, headSHA, triggerAnnotation string), func(context.Context) artifactsrc.Artifact, func(paused bool), error) {
	return n.build(ctx, nodeKey, advisorToken, drain, artifacts, appName, userID, chatID, nodeID, sink)
}

// perNodeServers lets shutdown close per-node A2A servers whose release() never ran; entries prune
// themselves on release, so memory stays bounded by nodes in flight.
type perNodeServers struct {
	mu   sync.Mutex
	open map[*agent.A2AServer]struct{}
}

func newPerNodeServers() *perNodeServers {
	return &perNodeServers{open: make(map[*agent.A2AServer]struct{})}
}

// track returns an idempotent release that untracks before closing, so closeAll never double-closes.
// The worker session outlives the dispatch (later reuse needs its history); only archive/delete reaps it.
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
