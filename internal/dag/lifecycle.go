package dag

import "sort"

// ActiveNodes returns chatID's mid-run node ids for the shutdown drain to pause, sorted so the log
// line and tests are deterministic.
func (e *Executor) ActiveNodes(chatID string) []string {
	e.controls.mu.Lock()
	defer e.controls.mu.Unlock()
	m := e.controls.m[chatID]
	out := make([]string, 0, len(m))
	for id := range m {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// MarkShutdown flags chatID's run as cut by the shutdown drain, so its abort
// leaves unfinished nodes for boot to resume instead of settling them cancelled.
func (e *Executor) MarkShutdown(chatID string) { e.controls.shutdown.Store(chatID, struct{}{}) }
