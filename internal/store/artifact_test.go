package store

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"

	"google.golang.org/adk/v2/artifact"
	"google.golang.org/genai"
)

// bothArtifactServices returns the row-backed GORM service and ADK's own
// in-memory service, proving parity against the semantics we must match.
func bothArtifactServices(t *testing.T) map[string]artifact.Service {
	t.Helper()
	st := newTestStore(t)
	row, err := NewRowArtifactService(st.db)
	if err != nil {
		t.Fatalf("NewRowArtifactService: %v", err)
	}
	return map[string]artifact.Service{
		"row (gorm/sqlite)": row,
		"in-memory (adk)":   artifact.InMemoryService(),
	}
}

func mustPart(text string) *genai.Part {
	return &genai.Part{InlineData: &genai.Blob{Data: []byte(text), MIMEType: "text/plain"}}
}

func TestArtifactService_SaveLoad_LatestByDefault(t *testing.T) {
	for name, svc := range bothArtifactServices(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			req := &artifact.SaveRequest{AppName: "app", UserID: "u", SessionID: "s", FileName: "f.txt", Part: mustPart("v1")}
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

			latest, err := svc.Load(ctx, &artifact.LoadRequest{AppName: "app", UserID: "u", SessionID: "s", FileName: "f.txt"})
			if err != nil {
				t.Fatalf("Load latest: %v", err)
			}
			if got := string(latest.Part.InlineData.Data); got != "v2" {
				t.Errorf("latest = %q, want %q", got, "v2")
			}

			v1, err := svc.Load(ctx, &artifact.LoadRequest{AppName: "app", UserID: "u", SessionID: "s", FileName: "f.txt", Version: 1})
			if err != nil {
				t.Fatalf("Load v1: %v", err)
			}
			if got := string(v1.Part.InlineData.Data); got != "v1" {
				t.Errorf("v1 = %q, want %q", got, "v1")
			}
		})
	}
}

func TestArtifactService_LoadMissing_NotExist(t *testing.T) {
	for name, svc := range bothArtifactServices(t) {
		t.Run(name, func(t *testing.T) {
			_, err := svc.Load(context.Background(), &artifact.LoadRequest{AppName: "app", UserID: "u", SessionID: "s", FileName: "missing.txt"})
			if !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("Load missing err = %v, want fs.ErrNotExist", err)
			}
		})
	}
}

func TestArtifactService_DeleteNonexistent_NotAnError(t *testing.T) {
	for name, svc := range bothArtifactServices(t) {
		t.Run(name, func(t *testing.T) {
			err := svc.Delete(context.Background(), &artifact.DeleteRequest{AppName: "app", UserID: "u", SessionID: "s", FileName: "never-existed.txt"})
			if err != nil {
				t.Errorf("Delete nonexistent = %v, want nil", err)
			}
		})
	}
}

func TestArtifactService_DeleteAllVersions(t *testing.T) {
	for name, svc := range bothArtifactServices(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			req := &artifact.SaveRequest{AppName: "app", UserID: "u", SessionID: "s", FileName: "f.txt", Part: mustPart("v1")}
			if _, err := svc.Save(ctx, req); err != nil {
				t.Fatalf("Save v1: %v", err)
			}
			req.Part = mustPart("v2")
			if _, err := svc.Save(ctx, req); err != nil {
				t.Fatalf("Save v2: %v", err)
			}

			if err := svc.Delete(ctx, &artifact.DeleteRequest{AppName: "app", UserID: "u", SessionID: "s", FileName: "f.txt"}); err != nil {
				t.Fatalf("Delete: %v", err)
			}
			if _, err := svc.Load(ctx, &artifact.LoadRequest{AppName: "app", UserID: "u", SessionID: "s", FileName: "f.txt"}); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("Load after delete-all err = %v, want fs.ErrNotExist", err)
			}
			if _, err := svc.Versions(ctx, &artifact.VersionsRequest{AppName: "app", UserID: "u", SessionID: "s", FileName: "f.txt"}); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("Versions after delete-all err = %v, want fs.ErrNotExist", err)
			}
		})
	}
}

func TestArtifactService_DeleteOneVersion_OthersSurvive(t *testing.T) {
	for name, svc := range bothArtifactServices(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			req := &artifact.SaveRequest{AppName: "app", UserID: "u", SessionID: "s", FileName: "f.txt", Part: mustPart("v1")}
			if _, err := svc.Save(ctx, req); err != nil {
				t.Fatalf("Save v1: %v", err)
			}
			req.Part = mustPart("v2")
			if _, err := svc.Save(ctx, req); err != nil {
				t.Fatalf("Save v2: %v", err)
			}

			if err := svc.Delete(ctx, &artifact.DeleteRequest{AppName: "app", UserID: "u", SessionID: "s", FileName: "f.txt", Version: 1}); err != nil {
				t.Fatalf("Delete v1: %v", err)
			}
			if _, err := svc.Load(ctx, &artifact.LoadRequest{AppName: "app", UserID: "u", SessionID: "s", FileName: "f.txt", Version: 1}); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("Load v1 after its delete err = %v, want fs.ErrNotExist", err)
			}
			latest, err := svc.Load(ctx, &artifact.LoadRequest{AppName: "app", UserID: "u", SessionID: "s", FileName: "f.txt"})
			if err != nil {
				t.Fatalf("Load latest (v2) after deleting v1: %v", err)
			}
			if got := string(latest.Part.InlineData.Data); got != "v2" {
				t.Errorf("surviving version = %q, want %q", got, "v2")
			}
		})
	}
}

func TestArtifactService_Versions(t *testing.T) {
	for name, svc := range bothArtifactServices(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			req := &artifact.SaveRequest{AppName: "app", UserID: "u", SessionID: "s", FileName: "f.txt", Part: mustPart("v1")}
			for _, text := range []string{"v1", "v2", "v3"} {
				req.Part = mustPart(text)
				if _, err := svc.Save(ctx, req); err != nil {
					t.Fatalf("Save %s: %v", text, err)
				}
			}

			vr, err := svc.Versions(ctx, &artifact.VersionsRequest{AppName: "app", UserID: "u", SessionID: "s", FileName: "f.txt"})
			if err != nil {
				t.Fatalf("Versions: %v", err)
			}
			got := map[int64]bool{}
			for _, v := range vr.Versions {
				got[v] = true
			}
			for _, want := range []int64{1, 2, 3} {
				if !got[want] {
					t.Errorf("Versions() = %v, missing %d", vr.Versions, want)
				}
			}
		})
	}
}

func TestArtifactService_List(t *testing.T) {
	for name, svc := range bothArtifactServices(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			for _, f := range []string{"a.txt", "b.txt"} {
				if _, err := svc.Save(ctx, &artifact.SaveRequest{AppName: "app", UserID: "u", SessionID: "s", FileName: f, Part: mustPart("x")}); err != nil {
					t.Fatalf("Save %s: %v", f, err)
				}
			}
			lr, err := svc.List(ctx, &artifact.ListRequest{AppName: "app", UserID: "u", SessionID: "s"})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(lr.FileNames) != 2 {
				t.Errorf("List() = %v, want 2 files", lr.FileNames)
			}
		})
	}
}

// User-scoped ("user:"-prefixed) filenames are visible from every session
// for the same app+user - mirrors ADK's own in-memory/gcsartifact services.
func TestArtifactService_UserScoped_VisibleAcrossSessions(t *testing.T) {
	for name, svc := range bothArtifactServices(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			if _, err := svc.Save(ctx, &artifact.SaveRequest{AppName: "app", UserID: "u", SessionID: "s1", FileName: "user:pref.txt", Part: mustPart("x")}); err != nil {
				t.Fatalf("Save: %v", err)
			}
			if _, err := svc.Load(ctx, &artifact.LoadRequest{AppName: "app", UserID: "u", SessionID: "s2", FileName: "user:pref.txt"}); err != nil {
				t.Errorf("Load from a different session = %v, want visible (user-scoped)", err)
			}
		})
	}
}

// TestArtifactService_RowBackend_SurvivesRestart is the durability-upgrade
// proof for sqlite: a fresh Store handle over the SAME db file (a cheap
// restart simulation) still loads an artifact saved by the OLD handle.
func TestArtifactService_RowBackend_SurvivesRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "quack.db")

	before, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New (before restart): %v", err)
	}
	svcBefore, err := NewRowArtifactService(before.db)
	if err != nil {
		t.Fatalf("NewRowArtifactService (before restart): %v", err)
	}
	ctx := context.Background()
	if _, err := svcBefore.Save(ctx, &artifact.SaveRequest{
		AppName: "app", UserID: "u", SessionID: "s", FileName: "f.txt", Part: mustPart("survives"),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Simulate a restart: a brand-new Store/gorm.DB handle over the same file.
	after, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New (after restart): %v", err)
	}
	svcAfter, err := NewRowArtifactService(after.db)
	if err != nil {
		t.Fatalf("NewRowArtifactService (after restart): %v", err)
	}
	loaded, err := svcAfter.Load(ctx, &artifact.LoadRequest{AppName: "app", UserID: "u", SessionID: "s", FileName: "f.txt"})
	if err != nil {
		t.Fatalf("Load after restart: %v", err)
	}
	if got := string(loaded.Part.InlineData.Data); got != "survives" {
		t.Errorf("loaded after restart = %q, want %q", got, "survives")
	}
}
