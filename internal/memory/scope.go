package memory

import (
	"context"
	"strings"

	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/session"

	"github.com/fagerbergj/quack/internal/ledger"
)

// Memory is SHARED and bucketed by subject, not siloed per agent.
const (
	RoleCoding   = "coding"
	RoleResearch = "research"
)

const (
	bucketRepo = "repo"
	bucketRole = "role"
	bucketUser = "user"
)

// Scope: caller's memory entitlement. Zero = nothing.
type Scope struct {
	Repo   string
	Role   string
	User   string
	Legacy string
}

// Buckets returns the keys this scope may READ, most specific first.
func (s Scope) Buckets() []string {
	out := make([]string, 0, 4)
	for _, b := range []string{
		prefixed(bucketRepo, s.Repo),
		prefixed(bucketRole, s.Role),
		prefixed(bucketUser, s.User),
		s.Legacy,
	} {
		if b != "" {
			out = append(out, b)
		}
	}
	return out
}

// writeBucket returns the key for `kind`; degrades repo→role→user so no context doesn't lose writes.
func (s Scope) writeBucket(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case bucketUser:
		if b := prefixed(bucketUser, s.User); b != "" {
			return b
		}
	case bucketRole:
		if b := prefixed(bucketRole, s.Role); b != "" {
			return b
		}
		return prefixed(bucketUser, s.User)
	}
	for _, b := range []string{
		prefixed(bucketRepo, s.Repo),
		prefixed(bucketRole, s.Role),
		prefixed(bucketUser, s.User),
	} {
		if b != "" {
			return b
		}
	}
	return ""
}

func prefixed(kind, key string) string {
	if strings.TrimSpace(key) == "" {
		return ""
	}
	return kind + ":" + key
}

// View is a Store bound to a Scope, implementing adkmemory.Service.
type View struct {
	store   *Store
	base    Scope
	resolve func(context.Context) Scope
	// led/chatID/nodeID (optional, set via WithRecall): when led is non-nil,
	// SearchMemory logs every call as a memory.recall ledger entry.
	led            ledger.LedgerStore
	chatID, nodeID string
}

var _ adkmemory.Service = (*View)(nil)

// View binds this store to a caller's Scope.
func (s *Store) View(base Scope, resolve func(context.Context) Scope) *View {
	return &View{store: s, base: base, resolve: resolve}
}

// WithRecall returns a copy of v that logs every SearchMemory call as a
// memory.recall ledger entry and bumps recalls on the delivered hits.
func (v *View) WithRecall(led ledger.LedgerStore, chatID, nodeID string) *View {
	c := *v
	c.led, c.chatID, c.nodeID = led, chatID, nodeID
	return &c
}

// Scope returns the caller's full entitlement for this call (base + resolved).
func (v *View) Scope(ctx context.Context) Scope {
	sc := v.base
	if v.resolve != nil {
		dyn := v.resolve(ctx)
		if dyn.Repo != "" {
			sc.Repo = dyn.Repo
		}
		if dyn.User != "" {
			sc.User = dyn.User
		}
		if dyn.Role != "" {
			sc.Role = dyn.Role
		}
	}
	return sc
}

// SearchMemory recalls across the caller's buckets; AppName/UserID is ignored.
func (v *View) SearchMemory(ctx context.Context, req *adkmemory.SearchRequest) (*adkmemory.SearchResponse, error) {
	if req == nil {
		return &adkmemory.SearchResponse{}, nil
	}
	resp, scoredHits, err := v.store.recall(ctx, v.Scope(ctx).Buckets(), req.Query)
	if err == nil && v.led != nil && resp != nil {
		v.store.LogRecall(ctx, v.led, v.chatID, v.nodeID, "tool", deliveredFrom(resp.Memories, scoredHits))
	}
	return resp, err
}

// AddSessionToMemory is a deliberate no-op (see Store.AddSessionToMemory).
func (v *View) AddSessionToMemory(context.Context, session.Session) error { return nil }
