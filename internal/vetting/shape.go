// Deterministic checks on the ANSWER'S SHAPE, wired into foldDeterministic
// exactly like mermaid.go's mermaidCriterion: no model judgement involved, a
// hard weakest-link fail when they fire. "The deliverable isn't a
// deliverable" slips past a judge that only scores content quality, never
// whether the text is even a well-formed answer.
package vetting

import (
	"fmt"
	"strings"
)

// toolCallSyntaxMarkers are fragments of tool-call wire syntax that can never
// legitimately appear in a delivered answer: a model that emitted a malformed
// or unparseable call sometimes leaks the raw fragment into its text response
// instead (#565 - the NightsOut#57 comment ended in a bare
// `</parameter></function></tool_call>`). Detecting the closing tags is enough
// - by the time one appears, the syntax has already leaked.
var toolCallSyntaxMarkers = []string{"</tool_call>", "</function>", "</parameter>"}

// toolCallSyntaxCriterion is the GATE side of #565: scans the answer plus every
// currently staged delivery body (same texts mermaidCriterion scans) for raw
// tool-call syntax. ok=false means nothing was found - a clean answer needs no
// entry, matching mermaidCriterion's convention.
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

// deliveryTexts is the answer plus every currently staged delivery body (the
// PR/review/comment text about to ship to GitHub) - the same text set
// mermaidCriterion scans, factored out so toolCallSyntaxCriterion shares it.
func deliveryTexts(answer string, act workerActivity) []string {
	texts := make([]string, 0, len(act.stagedDelivery)+1)
	texts = append(texts, answer)
	for _, sd := range act.stagedDelivery {
		texts = append(texts, sd.Body)
	}
	return texts
}
