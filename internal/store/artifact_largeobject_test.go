package store

import (
	"context"
	"errors"
	"io/fs"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/genai"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// newTestPostgresLargeObjectService starts a real Postgres container - the
// only way to exercise loBlobBackend (sqlite has no large objects). Skips
// (not fails) when Docker isn't reachable (CI's ubuntu-latest has it; local dev may not).
func newTestPostgresLargeObjectService(t *testing.T) artifact.Service {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ctr, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("quack_artifacts_test"),
		tcpostgres.WithUsername("quack"),
		tcpostgres.WithPassword("quack"),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("5432/tcp").WithStartupTimeout(60*time.Second)),
	)
	if err != nil {
		t.Skipf("docker unavailable, skipping large-object integration test: %v", err)
	}
	t.Cleanup(func() {
		if err := ctr.Terminate(context.Background()); err != nil {
			t.Logf("terminate postgres container: %v", err)
		}
	})

	dsn, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: slogGormLogger()})
	if err != nil {
		t.Fatalf("gorm.Open: %v", err)
	}
	svc, err := NewLargeObjectArtifactService(db)
	if err != nil {
		t.Fatalf("NewLargeObjectArtifactService: %v", err)
	}
	return svc
}

// TestArtifactService_LargeObjectBackend_Parity runs the row-backed suite's
// Save/Load/Versions/Delete cases (artifact_test.go) against a real Postgres backend.
func TestArtifactService_LargeObjectBackend_Parity(t *testing.T) {
	svc := newTestPostgresLargeObjectService(t)
	ctx := context.Background()

	req := &artifact.SaveRequest{AppName: "app", UserID: "u", SessionID: "s", FileName: "f.bin", Part: mustPart("v1")}
	r1, err := svc.Save(ctx, req)
	if err != nil {
		t.Fatalf("Save v1: %v", err)
	}
	if r1.Version != 1 {
		t.Errorf("v1 version = %d, want 1", r1.Version)
	}
	req.Part = mustPart("v2")
	r2, err := svc.Save(ctx, req)
	if err != nil {
		t.Fatalf("Save v2: %v", err)
	}
	if r2.Version != 2 {
		t.Errorf("v2 version = %d, want 2", r2.Version)
	}

	latest, err := svc.Load(ctx, &artifact.LoadRequest{AppName: "app", UserID: "u", SessionID: "s", FileName: "f.bin"})
	if err != nil {
		t.Fatalf("Load latest: %v", err)
	}
	if got := string(latest.Part.InlineData.Data); got != "v2" {
		t.Errorf("latest = %q, want %q", got, "v2")
	}
	v1, err := svc.Load(ctx, &artifact.LoadRequest{AppName: "app", UserID: "u", SessionID: "s", FileName: "f.bin", Version: 1})
	if err != nil {
		t.Fatalf("Load v1: %v", err)
	}
	if got := string(v1.Part.InlineData.Data); got != "v1" {
		t.Errorf("v1 = %q, want %q", got, "v1")
	}

	if err := svc.Delete(ctx, &artifact.DeleteRequest{AppName: "app", UserID: "u", SessionID: "s", FileName: "f.bin", Version: 1}); err != nil {
		t.Fatalf("Delete v1: %v", err)
	}
	if _, err := svc.Load(ctx, &artifact.LoadRequest{AppName: "app", UserID: "u", SessionID: "s", FileName: "f.bin", Version: 1}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Load v1 after its delete err = %v, want fs.ErrNotExist", err)
	}
	if err := svc.Delete(ctx, &artifact.DeleteRequest{AppName: "app", UserID: "u", SessionID: "s", FileName: "f.bin"}); err != nil {
		t.Fatalf("Delete all: %v", err)
	}
	if _, err := svc.Load(ctx, &artifact.LoadRequest{AppName: "app", UserID: "u", SessionID: "s", FileName: "f.bin"}); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Load after delete-all err = %v, want fs.ErrNotExist", err)
	}
	// Deleting an already-gone artifact is not an error (matches the row backend).
	if err := svc.Delete(ctx, &artifact.DeleteRequest{AppName: "app", UserID: "u", SessionID: "s", FileName: "f.bin"}); err != nil {
		t.Errorf("Delete already-deleted = %v, want nil", err)
	}
}

// TestArtifactService_LargeObjectBackend_BigPayload round-trips content well
// above the ~1KB TOAST threshold - a bytea-column mistake would still
// "work" at small sizes, but not here.
func TestArtifactService_LargeObjectBackend_BigPayload(t *testing.T) {
	svc := newTestPostgresLargeObjectService(t)
	ctx := context.Background()

	data := make([]byte, 5*1024*1024) // 5MiB
	for i := range data {
		data[i] = byte(i % 251)
	}
	req := &artifact.SaveRequest{AppName: "app", UserID: "u", SessionID: "s", FileName: "big.bin",
		Part: &genai.Part{InlineData: &genai.Blob{Data: data, MIMEType: "application/octet-stream"}}}
	_, err := svc.Save(ctx, req)
	if err != nil {
		t.Fatalf("Save big payload: %v", err)
	}
	loaded, err := svc.Load(ctx, &artifact.LoadRequest{AppName: "app", UserID: "u", SessionID: "s", FileName: "big.bin"})
	if err != nil {
		t.Fatalf("Load big payload: %v", err)
	}
	if len(loaded.Part.InlineData.Data) != len(data) {
		t.Fatalf("loaded %d bytes, want %d", len(loaded.Part.InlineData.Data), len(data))
	}
	for i := range data {
		if loaded.Part.InlineData.Data[i] != data[i] {
			t.Fatalf("byte %d corrupted: got %d want %d", i, loaded.Part.InlineData.Data[i], data[i])
		}
	}
}
