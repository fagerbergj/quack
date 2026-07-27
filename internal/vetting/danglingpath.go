package vetting

import (
	"fmt"
	"path/filepath"
	"strings"
)

// danglingDeliverablePathCriterion is the GATE side of #569: a plan-only run
// commits nothing, so any file the worker wrote (act.written) is discarded
// with the node's working directory the moment the run ends. An answer that
// then points to one of those exact paths as where the deliverable lives -
// "the plan is complete at PLAN_58_....md" - is broken by construction the
// instant it's written, since nothing downstream can ever reach that file.
//
// Scoped to act.written (paths THIS run's own write_file/edit_file calls
// touched) rather than any path-shaped text in the answer, so a legitimate
// citation of a file the worker only read - never wrote - is never flagged.
// !act.committed is what makes the file unreachable: a committed write ships
// on the branch/PR the delivery step pushes, so referencing it is fine.
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
