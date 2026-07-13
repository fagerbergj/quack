package memory

import (
	"context"
	"strings"

	adkmemory "google.golang.org/adk/v2/memory"
	"google.golang.org/adk/v2/session"
)

// Memory is SHARED and bucketed by SUBJECT, not siloed per agent. A memory belongs
// to a bucket describing WHAT IT IS ABOUT; an agent reads the union of the buckets
// it is entitled to:
//
//	repo:<repo>   the repository being worked on — conventions, build/test/lint
//	              commands, reference features, registration points, pre-existing
//	              failures. Read AND written by every coding agent (explorer,
//	              implementer, reviewer): what one learns, the next one gets.
//	role:<family> a role family's durable tradecraft ("coding" | "research") —
//	              how to do the job, independent of any one repo.
//	user:<id>     facts about the user. Read by everyone acting for that user.
//
// Before this, task memory was keyed by AGENT NAME: the explorer's hard-won repo
// knowledge never reached the implementer (which had no memory tools at all).
const (
	RoleCoding   = "coding"
	RoleResearch = "research"
)

// Bucket kinds, as named by stage_memory's `bucket` argument.
const (
	bucketRepo = "repo"
	bucketRole = "role"
	bucketUser = "user"
)

// Scope is one caller's memory entitlement: the buckets it may read, and the
// buckets its writes may land in. The zero value entitles nothing.
type Scope struct {
	// Repo identifies the repository the node is working in ("github.com/acme/games").
	// Empty when the deployment/node has no repo context — writes then fall back to
	// the role bucket rather than guessing a key.
	Repo string
	// Role is the caller's role family (RoleCoding | RoleResearch); empty = none.
	Role string
	// User is the real user id; empty = unknown (behind A2A the per-invocation
	// "A2A_USER_<ctxid>" is NOT a user id — resolve the real one or leave it empty).
	User string
	// Legacy is the pre-bucket scope key this caller owned: the agent name (task
	// memory) or the raw user id (user memory). Read-only, so memories written
	// before the bucket model still load. No migration, nothing lost — and nothing
	// new is ever written here.
	Legacy string
}

// Buckets returns the bucket keys this scope may READ, most specific first.
func (s Scope) Buckets() []string {
	out := make([]string, 0, 4)
	for _, b := range []string{
		prefixed(bucketRepo, s.Repo),
		prefixed(bucketRole, s.Role),
		prefixed(bucketUser, s.User),
		s.Legacy, // legacy points carry a raw, unprefixed scope
	} {
		if b != "" {
			out = append(out, b)
		}
	}
	return out
}

// writeBucket returns the bucket key a memory tagged `kind` (repo|role|user, or ""
// for the default) is WRITTEN to. A bucket the caller has no key for degrades to
// the next-broadest one it does — repo → role → user — so a deployment with no repo
// context still remembers instead of dropping the write. Never writes to Legacy.
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
	// repo (explicit) or unspecified: the default ladder.
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

// View is one caller's read view of a Store: a Store bound to a Scope, implementing
// adkmemory.Service so ADK's preload_memory / load_memory resolve through it. The
// Store itself is shared by every agent — the View is what makes a caller see only
// its own buckets.
//
// base carries what is known at build time (the agent's role family + its legacy
// agent-name scope). resolve, when set, supplies what is only knowable per call —
// the repo the node is working in and the real user id — from the invocation's
// context (see internal/serve). It may return the zero Scope: a caller with no repo
// context still reads its role and legacy buckets.
type View struct {
	store   *Store
	base    Scope
	resolve func(context.Context) Scope
}

var _ adkmemory.Service = (*View)(nil)

// View binds this store to a caller's scope.
func (s *Store) View(base Scope, resolve func(context.Context) Scope) *View {
	return &View{store: s, base: base, resolve: resolve}
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

// SearchMemory recalls across the union of this caller's buckets. The request's
// AppName/UserID are deliberately ignored: behind A2A they name the agent and a
// per-invocation user, which is exactly the per-agent siloing this replaces.
func (v *View) SearchMemory(ctx context.Context, req *adkmemory.SearchRequest) (*adkmemory.SearchResponse, error) {
	if req == nil {
		return &adkmemory.SearchResponse{}, nil
	}
	return v.store.recall(ctx, v.Scope(ctx).Buckets(), req.Query)
}

// AddSessionToMemory is a deliberate no-op (see Store.AddSessionToMemory).
func (v *View) AddSessionToMemory(context.Context, session.Session) error { return nil }
