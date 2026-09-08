package memory

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/testcontainers/testcontainers-go"
	tcqdrant "github.com/testcontainers/testcontainers-go/modules/qdrant"

	"google.golang.org/adk/v2/model"

	"github.com/google/uuid"
)

// qdrantTestAddr starts ONE Qdrant container for the whole `go test` process
// (fresh collections per test give isolation - a container per test would make
// this package the slowest thing in the suite) and returns its gRPC address.
// Skips (not fails) when Docker isn't reachable, matching the ledger's
// postgres container tests (#1237, internal/ledger/pgstore_test.go).
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
		// Pinned to roughly match the go-client version in go.mod (v1.19) -
		// CollectionExists against a too-old server 501s ("Unimplemented"). Raise
		// the container's nofile ulimit above Docker's 1024 default: RocksDB opens
		// several file handles per collection, and this suite creates one
		// collection per converted test on a single shared container.
		ctr, err := tcqdrant.Run(ctx, "qdrant/qdrant:v1.12.4",
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

// newQdrantStore builds a Store against the shared test container, on a fresh
// collection per call - qdrantCollSeq (not t.Name()) because forEachBackend
// subtests share one process-wide counter and a name alone can collide across
// t.Run("qdrant", ...) call sites using the same domain.
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

// testID maps a readable test fixture name (e.g. "m1", "verified-old") to a
// stable UUID: production memory ids are always uuid.NewString() (commit.go),
// and Qdrant's point-id wire type rejects anything that doesn't parse as a
// UUID or uint64 ("Unable to parse UUID") - sqlite has no such constraint, so
// this only bites once a test runs against both backends via forEachBackend.
// Deterministic (uuid.NewSHA1) so failure messages built from the name still
// read the same across runs.
func testID(name string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(name)).String()
}

// forEachBackend runs run against a fresh Store on both the sqlite and qdrant
// indexes, as a subtest per backend - the shared Store logic (votes, tiers,
// absorption, rescope, forgetting, recall) is index-agnostic and #1268 wants it
// proved against both, not just the always-on sqlite path.
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
