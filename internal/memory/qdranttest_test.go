package memory

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/qdrant/go-client/qdrant"
	"github.com/testcontainers/testcontainers-go"
	tcqdrant "github.com/testcontainers/testcontainers-go/modules/qdrant"

	"google.golang.org/adk/v2/model"

	"github.com/google/uuid"
)

// qdrantTestAddr starts ONE Qdrant container for the whole `go test` process (fresh
// collections per test give isolation - a container per test would make this package
// the slowest thing in the suite) and returns its gRPC address. Skips (not fails) when Docker isn't reachable, matching the ledger's postgres container tests (#1237, internal/ledger/pgstore_test.go).
var (
	qdrantAddrOnce sync.Once
	qdrantAddr     string
	qdrantAddrErr  error
	qdrantCollSeq  atomic.Int64
)

func qdrantTestAddr(t *testing.T) string {
	t.Helper()
	qdrantAddrOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		// Pinned to match prod's server exactly (v1.19.1) - a too-old server (was v1.12.4)
		// sends the legacy VectorOutput.Data wire shape and can't catch a client
		// that only reads the new Dense oneof arm, or vice versa (#1269/#1268). Raise the container's nofile ulimit above Docker's 1024 default: RocksDB opens several file handles per collection, and this suite creates one collection per converted test on a single shared container.
		ctr, err := tcqdrant.Run(ctx, "qdrant/qdrant:v1.19.1",
			testcontainers.WithHostConfigModifier(func(hc *container.HostConfig) {
				hc.Ulimits = []*container.Ulimit{{Name: "nofile", Soft: 65536, Hard: 65536}}
			}),
		)
		if err != nil {
			qdrantAddrErr = err
			return
		}
		addr, err := ctr.GRPCEndpoint(ctx)
		if err != nil {
			qdrantAddrErr = err
			return
		}
		qdrantAddr = addr
	})
	if qdrantAddrErr != nil {
		t.Skipf("docker unavailable, skipping qdrant memory integration test: %v", qdrantAddrErr)
	}
	return qdrantAddr
}

// newQdrantStore builds a Store against the shared test container, on a fresh collection
// per call - qdrantCollSeq (not t.Name()) because forEachBackend subtests share one
// process-wide counter and a name alone can collide across t.Run("qdrant", ...) call sites using the same domain.
func newQdrantStore(t *testing.T, domain string, consolidator model.LLM) *Store {
	t.Helper()
	addr := qdrantTestAddr(t)
	coll := fmt.Sprintf("test_%s_%d", domain, qdrantCollSeq.Add(1))
	s, err := Open(context.Background(), addr, fakeEmbedder{}, consolidator, coll, domain, 5, 0.5)
	if err != nil {
		t.Fatalf("Open (qdrant): %v", err)
	}
	return s
}

// TestQdrant_ListWithVectorsReturnsNonEmptyVector is the regression guard for #1268/#1269:
// a v1.19 server answers with the newer VectorOutput.Dense oneof arm, not the
// deprecated top-level Data field vectorData used to read alone, which silently made every retrieved vector nil (cosine 0 everywhere, so DedupeSweep and the MMR re-rank never found any pair similar).
func TestQdrant_ListWithVectorsReturnsNonEmptyVector(t *testing.T) {
	ctx := context.Background()
	s := newQdrantStore(t, "task", nil)
	seedMemory(t, s, point{ID: testID("v"), Content: "vector round trip", Scope: "repo:x", Vector: []float32{1, 0, 0, 0}})

	got, err := s.idx.list(ctx, []string{"repo:x"}, 0, 10, false, "", true)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("list returned %d points, want 1", len(got))
	}
	if len(got[0].Vector) != 4 {
		t.Fatalf("vector = %v, want a non-empty 4-dim vector", got[0].Vector)
	}
}

// TestQdrant_ListPagingTiesExactlyOnce is the adversarial-review regression for
// listOrdered: Qdrant's order_by has one sort key and no secondary column, and its
// own docs warn ties on a non-unique field give no ID-offset guarantee - a naive per-call Scroll(limit=offset+limit) can include a different, unstable subset of a tied group each time it's called. Seeds a group that all share ONE timestamp (bigger than one page) and pages through with independent List calls - each its own fresh Scroll, exactly how a real "next page" click works, not a shared cursor - checking every point surfaces exactly once.
func TestQdrant_ListPagingTiesExactlyOnce(t *testing.T) {
	ctx := context.Background()
	s := newQdrantStore(t, "task", nil)
	const n = 23
	const tied = "2026-08-01T00:00:00Z"
	for i := 0; i < n; i++ {
		seedMemory(t, s, point{
			ID: testID(fmt.Sprintf("tie-%d", i)), Content: fmt.Sprintf("fact %d", i),
			Scope: "repo:tie", Timestamp: tied,
		})
	}

	const pageSize = 7
	seen := map[string]int{}
	for offset := 0; offset < n+pageSize; offset += pageSize {
		page, total, err := s.List(ctx, []string{"repo:tie"}, offset, pageSize, false, "")
		if err != nil {
			t.Fatalf("List offset=%d: %v", offset, err)
		}
		if total != n {
			t.Fatalf("total at offset=%d = %d, want %d", offset, total, n)
		}
		for _, m := range page {
			seen[m.ID]++
		}
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("id %s appeared %d times across pages, want exactly 1", id, count)
		}
	}
	if len(seen) != n {
		t.Fatalf("saw %d distinct ids across all pages, want %d (no omissions)", len(seen), n)
	}
}

// TestQdrant_ListPagingLargeTieExactlyOnce is the 500-point scale variant of the tie-group
// regression above: a tie group larger than resolveTieBoundary's typical case (23) by an
// order of magnitude, still on a small page size, so nearly every page triggers the full-group refetch. Confirms the refetch stays correct (exactly once, no omissions) at this scale, not just small n.
func TestQdrant_ListPagingLargeTieExactlyOnce(t *testing.T) {
	ctx := context.Background()
	s := newQdrantStore(t, "task", nil)
	const n = 500
	const tied = "2026-08-01T00:00:00Z"
	for i := 0; i < n; i++ {
		seedMemory(t, s, point{
			ID: testID(fmt.Sprintf("bigtie-%d", i)), Content: fmt.Sprintf("fact %d", i),
			Scope: "repo:bigtie", Timestamp: tied,
		})
	}

	const pageSize = 50
	seen := map[string]int{}
	for offset := 0; offset < n+pageSize; offset += pageSize {
		page, total, err := s.List(ctx, []string{"repo:bigtie"}, offset, pageSize, false, "")
		if err != nil {
			t.Fatalf("List offset=%d: %v", offset, err)
		}
		if total != n {
			t.Fatalf("total at offset=%d = %d, want %d", offset, total, n)
		}
		for _, m := range page {
			seen[m.ID]++
		}
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("id %s appeared %d times across pages, want exactly 1", id, count)
		}
	}
	if len(seen) != n {
		t.Fatalf("saw %d distinct ids across all pages, want %d (no omissions)", len(seen), n)
	}
}

// testID maps a readable test fixture name (e.g. "m1", "verified-old") to a stable UUID:
// production memory ids are always uuid.NewString() (commit.go), and Qdrant's point-id
// wire type rejects anything that doesn't parse as a UUID or uint64 ("Unable to parse UUID") - sqlite has no such constraint, so this only bites once a test runs against both backends via forEachBackend. Deterministic (uuid.NewSHA1) so failure messages built from the name still read the same across runs.
func testID(name string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(name)).String()
}

// forEachBackend runs run against a fresh Store on both the sqlite and qdrant indexes,
// as a subtest per backend - the shared Store logic (votes, tiers, absorption,
// rescope, forgetting, recall) is index-agnostic and #1268 wants it proved against both, not just the always-on sqlite path.
func forEachBackend(t *testing.T, run func(t *testing.T, newStore func(domain string, consolidator model.LLM) *Store)) {
	t.Helper()
	backends := []struct {
		name string
		ctor func(*testing.T, string, model.LLM) *Store
	}{
		{"sqlite", newSQLiteStore},
		{"qdrant", newQdrantStore},
	}
	for _, b := range backends {
		b := b
		t.Run(b.name, func(t *testing.T) {
			run(t, func(domain string, consolidator model.LLM) *Store { return b.ctor(t, domain, consolidator) })
		})
	}
}

// TestQdrant_EnsureTimestampIndex_AddsToPreexistingCollection is the startup half of the
// order_by fix: a collection created before ensureTimestampIndex existed (or by any
// other client) has no payload index on `timestamp`, and order_by needs one. ensure() must add it on the pre-existing-collection branch, not just the brand-new-collection one, and calling it again on an already-indexed collection must stay a no-op.
func TestQdrant_EnsureTimestampIndex_AddsToPreexistingCollection(t *testing.T) {
	addr := qdrantTestAddr(t)
	host, port, err := parseAddr(addr)
	if err != nil {
		t.Fatalf("parseAddr: %v", err)
	}
	client, err := qdrant.NewClient(&qdrant.Config{Host: host, Port: port, SkipCompatibilityCheck: true})
	if err != nil {
		t.Fatalf("qdrant.NewClient: %v", err)
	}
	coll := fmt.Sprintf("test_preexisting_%d", qdrantCollSeq.Add(1))
	if err := client.CreateCollection(context.Background(), &qdrant.CreateCollection{
		CollectionName: coll,
		VectorsConfig:  qdrant.NewVectorsConfig(&qdrant.VectorParams{Size: 4, Distance: qdrant.Distance_Cosine}),
	}); err != nil {
		t.Fatalf("CreateCollection: %v", err)
	}
	info, err := client.GetCollectionInfo(context.Background(), coll)
	if err != nil {
		t.Fatalf("GetCollectionInfo: %v", err)
	}
	if _, ok := info.GetPayloadSchema()[payloadTimestamp]; ok {
		t.Fatal("test setup: pre-existing collection already has a timestamp index")
	}

	idx := &qdrantIndex{client: client, coll: coll}
	probeDim := func() (int, error) { return 4, nil }
	if err := idx.ensure(context.Background(), probeDim); err != nil {
		t.Fatalf("ensure (pre-existing collection): %v", err)
	}
	info, err = client.GetCollectionInfo(context.Background(), coll)
	if err != nil {
		t.Fatalf("GetCollectionInfo after ensure: %v", err)
	}
	if _, ok := info.GetPayloadSchema()[payloadTimestamp]; !ok {
		t.Fatal("ensure did not add a timestamp payload index to the pre-existing collection")
	}

	// Idempotent: a second call against an already-indexed collection must
	// not error (CreateFieldIndex on a duplicate index would).
	if err := idx.ensure(context.Background(), probeDim); err != nil {
		t.Fatalf("ensure (already indexed): %v", err)
	}
}
