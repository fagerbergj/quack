package ledger

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestFSStoreAppendReadOrder(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFSStore(dir)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	ctx := context.Background()
	want := []string{`{"seq":1}`, `{"seq":2}`, `{"seq":3}`}
	for _, entry := range want {
		if err := s.Append(ctx, "chat-1", []byte(entry)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	r, err := s.ReadStream(ctx, "chat-1")
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	defer r.Close()

	var got []string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		got = append(got, sc.Text())
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q (append order not preserved)", i, got[i], want[i])
		}
	}
}

func TestFSStoreConcurrentAppendNeverInterleaves(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFSStore(dir)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	ctx := context.Background()

	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			entry, _ := json.Marshal(map[string]int{"seq": i})
			if err := s.Append(ctx, "chat-concurrent", entry); err != nil {
				t.Errorf("Append: %v", err)
			}
		}(i)
	}
	wg.Wait()

	r, err := s.ReadStream(ctx, "chat-concurrent")
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	defer r.Close()

	sc := bufio.NewScanner(r)
	count := 0
	for sc.Scan() {
		var m map[string]int
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line %d is not valid JSON (interleaved write): %q: %v", count, sc.Text(), err)
		}
		count++
	}
	if count != n {
		t.Fatalf("got %d lines, want %d", count, n)
	}
}

// TestFSStoreCrashTail simulates a kill -9 mid-write: a valid line followed
// by a truncated fragment with no trailing newline. Reading back must
// surface the complete lines and tolerate the partial tail rather than
// erroring the whole stream.
func TestFSStoreCrashTail(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFSStore(dir)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	ctx := context.Background()
	if err := s.Append(ctx, "chat-crash", []byte(`{"seq":1}`)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Append(ctx, "chat-crash", []byte(`{"seq":2}`)); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// Append a partial line directly (bypassing Append's newline-terminated
	// write) to simulate a write that was cut off mid-record.
	p := filepath.Join(dir, "chat-crash.jsonl")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString(`{"seq":3,"trunca`); err != nil {
		t.Fatalf("write partial: %v", err)
	}
	f.Close()

	r, err := s.ReadStream(ctx, "chat-crash")
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	defer r.Close()

	var valid int
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err == nil {
			valid++
		}
		// An unparseable last line is EXPECTED here (the simulated crash tail)
		// and must not abort the scan or the test - the property under test is
		// that the two complete lines are still readable.
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan error: %v", err)
	}
	if valid != 2 {
		t.Fatalf("got %d valid (complete) lines, want 2", valid)
	}
}

func TestFSStoreListAndDelete(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFSStore(dir)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	ctx := context.Background()
	for _, id := range []string{"a", "b"} {
		if err := s.Append(ctx, id, []byte(`{}`)); err != nil {
			t.Fatalf("Append(%s): %v", id, err)
		}
	}
	refs, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("got %d sessions, want 2: %+v", len(refs), refs)
	}
	if err := s.Delete(ctx, "a"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	refs, err = s.List(ctx)
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	if len(refs) != 1 || refs[0].ID != "b" {
		t.Fatalf("got %+v, want only session b", refs)
	}
	// Deleting an already-gone session is a no-op, not an error.
	if err := s.Delete(ctx, "a"); err != nil {
		t.Fatalf("Delete (already gone): %v", err)
	}
}

func TestFSStoreRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFSStore(dir)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	ctx := context.Background()
	for _, bad := range []string{"", ".", "..", "../escape", "a/b"} {
		if err := s.Append(ctx, bad, []byte(`{}`)); err == nil {
			t.Errorf("Append(%q) succeeded, want a rejection", bad)
		}
	}
}

// TestFSStoreReadEntriesSkipsNonIntentLinesAndRespectsFromSeq mixes an
// OTel-shaped line (no chat_id/kind) with AppendIntent entries, and checks
// ReadEntries returns only the entries, honoring fromSeq.
func TestFSStoreReadEntriesSkipsNonIntentLinesAndRespectsFromSeq(t *testing.T) {
	dir := t.TempDir()
	s, err := NewFSStore(dir)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	ctx := context.Background()
	const chatID = "chat-mixed"

	if err := s.Append(ctx, chatID, []byte(`{"body":"otel line","attributes":{}}`)); err != nil {
		t.Fatalf("Append otel line: %v", err)
	}
	for range 3 {
		if _, err := s.AppendIntent(ctx, Entry{ChatID: chatID, Kind: KindNodeStarted}); err != nil {
			t.Fatalf("AppendIntent: %v", err)
		}
	}

	all, err := s.ReadEntries(ctx, chatID, 0)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("got %d entries, want 3 (otel line skipped): %+v", len(all), all)
	}
	for i, e := range all {
		if e.Seq != int64(i+1) {
			t.Errorf("entries[%d].Seq = %d, want %d", i, e.Seq, i+1)
		}
	}

	fromTwo, err := s.ReadEntries(ctx, chatID, 2)
	if err != nil {
		t.Fatalf("ReadEntries fromSeq=2: %v", err)
	}
	if len(fromTwo) != 2 || fromTwo[0].Seq != 2 {
		t.Errorf("ReadEntries fromSeq=2 = %+v, want seq 2 and 3", fromTwo)
	}
}

// TestFSStoreReadEntriesMissingFileReturnsNilNil: a chat with no file at
// all (never appended to) is (nil, nil), not an error - ReadEntries is a
// projection reader, and "no entries yet" isn't a failure.
func TestFSStoreReadEntriesMissingFileReturnsNilNil(t *testing.T) {
	s, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	entries, err := s.ReadEntries(context.Background(), "never-touched", 0)
	if err != nil {
		t.Fatalf("ReadEntries: %v", err)
	}
	if entries != nil {
		t.Errorf("entries = %+v, want nil", entries)
	}
}

// TestFSStoreAppendIntentSeqSurvivesRestart: a fresh FSStore instance
// pointed at an existing file (simulating a process restart) must seed its
// in-memory counter from the file's own max seq, not restart at 1 and
// collide with what's already on disk.
func TestFSStoreAppendIntentSeqSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	const chatID = "chat-restart"

	first, err := NewFSStore(dir)
	if err != nil {
		t.Fatalf("NewFSStore: %v", err)
	}
	for range 3 {
		if _, err := first.AppendIntent(ctx, Entry{ChatID: chatID, Kind: KindNodeStarted}); err != nil {
			t.Fatalf("AppendIntent: %v", err)
		}
	}

	// A second FSStore over the same root has its own (empty) in-memory
	// counter - the restart this simulates.
	second, err := NewFSStore(dir)
	if err != nil {
		t.Fatalf("NewFSStore (restart): %v", err)
	}
	seq, err := second.AppendIntent(ctx, Entry{ChatID: chatID, Kind: KindNodeDone})
	if err != nil {
		t.Fatalf("AppendIntent after restart: %v", err)
	}
	if seq != 4 {
		t.Errorf("seq after restart = %d, want 4 (continuing past the file's existing 1..3)", seq)
	}
}
