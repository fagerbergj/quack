package remarkable

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStateLoadMissingFileReturnsEmpty(t *testing.T) {
	st, err := loadState(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	if st.Documents == nil || len(st.Documents) != 0 {
		t.Fatalf("Documents = %+v, want empty non-nil map", st.Documents)
	}
}

func TestStateSaveAndReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	mod := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

	st := &state{Documents: map[string]docState{
		"doc-1": {ID: "doc-1", Name: "Notes", LastModified: mod, LastOutcome: "done", Attempts: 1},
	}}

	if err := st.save(path); err != nil {
		t.Fatalf("save: %v", err)
	}

	reloaded, err := loadState(path)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	got, ok := reloaded.Documents["doc-1"]
	if !ok {
		t.Fatal("doc-1 missing after reload")
	}
	if got.Name != "Notes" || got.LastOutcome != "done" || got.Attempts != 1 {
		t.Errorf("reloaded doc-1 = %+v", got)
	}
	if !got.LastModified.Equal(mod) {
		t.Errorf("LastModified = %v, want %v", got.LastModified, mod)
	}
}
