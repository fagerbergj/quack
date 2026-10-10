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

// One Qdrant container per test process (fresh collection per test); a container per test would be
// too slow. Skips, not fails, without Docker.
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
		// Pinned to prod's v1.19.1: older servers send the legacy VectorOutput.Data shape. nofile is raised
		// above Docker's 1024 because RocksDB opens several handles per collection.
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

// qdrantCollSeq, not t.Name(): forEachBackend subtests with the same domain could collide on name.
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

// v1.19 answers with the VectorOutput.Dense arm; reading only Data made every vector nil, so dedupe
// and MMR never found a similar pair.
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

// order_by has no secondary key, so ties get no offset guarantee. A tie group bigger than a page,
// paged with independent List calls like real "next page" clicks, must surface each point once.
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

// 500-point tie group on a small page: nearly every page triggers the full-group refetch.
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

// Qdrant point ids must parse as UUID or uint64, so fixture names map to deterministic UUIDs
// (uuid.NewSHA1) that read the same across runs.
func testID(name string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(name)).String()
}

// forEachBackend runs run as a subtest on both sqlite and qdrant; shared Store logic is index-agnostic.
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

// order_by needs a `timestamp` payload index: ensure() must add it to a pre-existing collection too,
// and stay a no-op on one already indexed.
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
