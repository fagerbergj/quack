package vetting

import (
	"fmt"
	"path/filepath"
	"strings"
)

// danglingDeliverablePathCriterion catches an answer pointing to a path this
// run wrote but never committed (act.written, !act.committed) - the node's
// working directory is discarded at run end, so nothing downstream can ever
// reach that file. Scoped to the run's OWN writes, never any path-shaped
// text, so a legitimate citation of a file only read stays unflagged.
func danglingDeliverablePathCriterion(answer string, act workerActivity) (criterionScore, bool) {
	if act.committed {
		return criterionScore{}, false
	}
	for _, w := range act.written {
		base := filepath.Base(w)
		if base == "" || base == "." || base == "/" {
			continue
		}
		if strings.Contains(answer, base) {
			return criterionScore{Score: 0, Reason: fmt.Sprintf(
				"deterministic: the answer points to %q as the deliverable, but it was only written to this run's "+
					"discarded working directory - never committed, so it exists nowhere the reader can reach it. "+
					"Your answer TEXT is the deliverable; state the result there instead of pointing at a file.", base)}, true
		}
	}
	return criterionScore{}, false
}
