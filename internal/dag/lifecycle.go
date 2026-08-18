package dag

import "sort"

// ActiveNodes returns the node ids with a live control registered for chatID,
// i.e. the nodes actually mid-run right now. The shutdown drain
// (serve.DrainActiveRuns) enumerates these to pause them; sorted so the log
// line and the tests are deterministic.
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
