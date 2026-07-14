package tools

import (
	"crypto/sha256"
	"fmt"
	"sync"
)

// maxSameReads is how many times a node may read the SAME file, unchanged, before
// read_file stops handing it back and tells the model to change tactics.
//
// THE LIVE FAILURE (code-mode dogfood, 2026-07-13). A code-implementer ran for 25
// minutes, made 98 tool calls, and wrote NOTHING. 41% of its calls were repeats:
//
//	x10  read_file  quack/internal/tools/registry.go
//	x9   read_file  quack/internal/tools/guard.go
//	x7   read_file  quack/internal/tools/fs.go
//
// It read registry.go TEN TIMES. Not exploration — amnesia. Its session had grown to
// ~166,000 tokens against a 65,536-token window, so compaction summarised the older
// turns away on every single turn. That deleted the file contents it had just read, so
// it read them again, which blew the budget again, so they were dropped again. A
// perfect thrash loop: read, forget, re-read, forget. It could never accumulate enough
// context to start writing, and it would have spun until it was killed.
//
// Handing the file over an eleventh time cannot help — the context has no room for it.
// Breaking the loop, and naming the three ways out, can.
const maxSameReads = 3

// readTracker counts identical reads per (chat, node, path). Content-keyed: a file that
// CHANGED resets its count, because re-reading a file you just edited is exactly right
// and must never be blocked.
type readTracker struct {
	mu sync.Mutex
	m  map[string]readState
}

type readState struct {
	sum   [32]byte
	count int
}

func newReadTracker() *readTracker { return &readTracker{m: map[string]readState{}} }

// observe records a read of path (whose content hashes to sum) by a node, and reports
// how many times that node has now read this exact content in a row.
func (t *readTracker) observe(chatID, nodeID, path string, content []byte) int {
	if t == nil {
		return 1
	}
	sum := sha256.Sum256(content)
	key := chatID + "\x00" + nodeID + "\x00" + path

	t.mu.Lock()
	defer t.mu.Unlock()
	st, ok := t.m[key]
	if !ok || st.sum != sum {
		t.m[key] = readState{sum: sum, count: 1} // new file, or it changed: start over
		return 1
	}
	st.count++
	t.m[key] = st
	return st.count
}

// rereadDirective is what a node gets instead of the file, once it has read the same
// unchanged bytes maxSameReads times. It is an INSTRUCTION, not a diagnostic: the model
// is in a loop it cannot see, and needs to be told what to do differently.
func rereadDirective(path string, n int) error {
	return fmt.Errorf("read_file: you have already read %q %d times and its content has not changed. "+
		"Re-reading it is not working: your context is full, so each read is summarised away again before "+
		"you can act on it. Do NOT read this file again. Instead:\n"+
		"  1. WRITE. `edit_file` replaces an exact snippet — it does not need the whole file in your context. "+
		"If you know the change you want, make it NOW.\n"+
		"  2. Or read a NARROW slice: `read_file` with `offset` and `limit` (tens of lines, not hundreds).\n"+
		"  3. Or `grep` for the exact symbol you need instead of re-reading the file around it.\n"+
		"You have enough to act. Act.", path, n)
}
