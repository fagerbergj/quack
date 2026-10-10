// Deterministic checks on the answer's shape, folded in like mermaidCriterion: a judge scoring content
// never notices that the text isn't even a well-formed answer.
package vetting

import (
	"fmt"
	"strings"
)

// toolCallSyntaxMarkers: tool-call wire fragments a malformed call can leak into the answer text.
// The closing tags suffice: by the time one appears, the syntax has leaked.
var toolCallSyntaxMarkers = []string{"</tool_call>", "</function>", "</parameter>"}

// toolCallSyntaxCriterion scans the answer and staged delivery bodies for raw tool-call syntax.
// ok=false means nothing was found.
func toolCallSyntaxCriterion(answer string, act workerActivity) (criterionScore, bool) {
	for _, t := range deliveryTexts(answer, act) {
		for _, marker := range toolCallSyntaxMarkers {
			if strings.Contains(t, marker) {
				return criterionScore{Score: 0, Reason: fmt.Sprintf(
					"deterministic: the answer contains raw tool-call syntax (%q) - a leaked or malformed tool call, "+
						"never valid deliverable text. Write a plain-prose answer with no tool-call fragments.", marker)}, true
			}
		}
	}
	return criterionScore{}, false
}

// deliveryTexts: the answer plus every staged delivery body about to ship, the set mermaidCriterion
// also scans.
func deliveryTexts(answer string, act workerActivity) []string {
	texts := make([]string, 0, len(act.stagedDelivery)+1)
	texts = append(texts, answer)
	for _, sd := range act.stagedDelivery {
		texts = append(texts, sd.Body)
	}
	return texts
}
