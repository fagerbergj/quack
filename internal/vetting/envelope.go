package vetting

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
)

// marshalEnvelope renders the envelope as indented JSON for the revise prompt. It falls back to a
// terse error string: a malformed envelope must not crash the revise round.
func marshalEnvelope(env verdictEnvelope) string {
	b, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return `{"error": "failed to render verdict envelope"}`
	}
	return string(b)
}

// marshalNotes renders a judge_round record's notes for the revise prompt, with the same fallback.
func marshalNotes(notes []JudgeNote) string {
	b, err := json.MarshalIndent(notes, "", "  ")
	if err != nil {
		return `{"error": "failed to render notes"}`
	}
	return string(b)
}

// scaleSpec: a criterion's score range. Judge criteria are 0-3; deterministic checks like
// cites_sources keep their native 0-1.
type scaleSpec struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

// bandSpec: one scoring band, sortable/validatable rather than a score-keyed string.
type bandSpec struct {
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
	Meaning string  `json:"meaning"`
}

// criterionSpec: what describes a criterion. Never carries a score - scores
// live one level up in failureEntry/passingEntry.
type criterionSpec struct {
	Name       string     `json:"name"`
	Definition string     `json:"definition,omitempty"`
	Scale      *scaleSpec `json:"scale,omitempty"`
	Bands      []bandSpec `json:"bands,omitempty"`
	// RequireFixOnFail: judge-orchestration only, never shown to the worker.
	RequireFixOnFail bool `json:"-"`
	// Deterministic: code owns this criterion; a judge-submitted score for it is dropped.
	Deterministic bool `json:"-"`
}

// anchorSpec: where in the answer a criticism points; kind decides which other fields apply.
type anchorSpec struct {
	// Kind is optional on the way in: judges near-miss it (trace 9ea8cbee), so
	// aggregateVerdict infers it from whichever payload field is set.
	Kind string `json:"kind,omitempty"` // quote | path | omission

	Text string `json:"text,omitempty"` // quote

	Path string `json:"path,omitempty"` // path
	Line int    `json:"line,omitempty"` // path

	Expected string `json:"expected,omitempty"` // omission
}

// evidenceItem: one deterministic-check data point backing a failure (e.g. a citation score).
type evidenceItem struct {
	Ref   string  `json:"ref"`
	Score float64 `json:"score"`
	Why   string  `json:"why,omitempty"`
}

// failureEntry: one failed criterion. Deterministic failures set Evidence, judge failures set Anchor.
type failureEntry struct {
	Criterion criterionSpec  `json:"criterion"`
	Score     float64        `json:"score"`
	Threshold float64        `json:"threshold"`
	Evidence  []evidenceItem `json:"evidence,omitempty"`
	Anchor    *anchorSpec    `json:"anchor,omitempty"`
	Shortfall string         `json:"shortfall,omitempty"`
	Fix       string         `json:"fix,omitempty"`
}

// passingEntry: a criterion that cleared its threshold. criterion carries only its name.
type passingEntry struct {
	Criterion criterionSpec `json:"criterion"`
	Score     float64       `json:"score"`
	Threshold float64       `json:"threshold"`
}

// verdictEnvelope: the structured feedback a worker gets for a failed round.
type verdictEnvelope struct {
	Passed    bool    `json:"passed"`
	Score     float64 `json:"score"`
	Threshold float64 `json:"threshold"`
	// Scoring is always "lowest_criterion" (weakest-link gating).
	Scoring               string         `json:"scoring"`
	Round                 int            `json:"round"`
	DeterministicFailures []failureEntry `json:"deterministic_failures,omitempty"`
	JudgeFailures         []failureEntry `json:"judge_failures,omitempty"`
	Passing               []passingEntry `json:"passing,omitempty"`
}

const scoringLowestCriterion = "lowest_criterion"

// criterionText prefers Shortfall over Reason. aggregateVerdict only copies Reason into Shortfall, so
// a judge that submits only shortfall must still be readable.
func criterionText(c criterionScore) string {
	if s := strings.TrimSpace(c.Shortfall); s != "" {
		return c.Shortfall
	}
	return c.Reason
}

// formatCriteriaDetail renders the DEBUG per-criterion line. Score is 0-1, so %.0f would collapse 0.9
// and 1.0 and hide the score compression this log exists to surface.
func formatCriteriaDetail(criteria map[string]criterionScore) string {
	parts := make([]string, 0, len(criteria))
	for name, cs := range criteria {
		parts = append(parts, fmt.Sprintf("%s=%.2f (%s)", name, cs.Score, strings.TrimSpace(criterionText(cs))))
	}
	sort.Strings(parts)
	return strings.Join(parts, " | ")
}

// buildEnvelope turns a verdict into the structured envelope. mergeDeterministic marks deterministic
// failures; anything else below threshold is a judge failure.
func buildEnvelope(v verdict, threshold float64, round int) verdictEnvelope {
	env := verdictEnvelope{
		Passed:    v.Score >= threshold,
		Score:     v.Score,
		Threshold: threshold,
		Scoring:   scoringLowestCriterion,
		Round:     round,
	}
	names := make([]string, 0, len(v.Criteria))
	for name := range v.Criteria {
		names = append(names, name)
	}
	sort.Strings(names) // stable order across runs (map iteration is random)
	for _, name := range names {
		c := v.Criteria[name]
		spec := criterionSpec{Name: name, Definition: c.Definition, Bands: c.Bands}
		if c.Scale != nil {
			spec.Scale = c.Scale
		}
		if c.Score >= threshold {
			env.Passing = append(env.Passing, passingEntry{
				Criterion: criterionSpec{Name: name},
				Score:     c.Score,
				Threshold: threshold,
			})
			continue
		}
		entry := failureEntry{
			Criterion: spec,
			Score:     c.Score,
			Threshold: threshold,
			Anchor:    c.Anchor,
			Shortfall: criterionText(c), // prefers Shortfall, falls back to the deprecated Reason
			Fix:       c.Fix,
		}
		if c.Deterministic {
			entry.Evidence = c.Evidence
			env.DeterministicFailures = append(env.DeterministicFailures, entry)
		} else {
			env.JudgeFailures = append(env.JudgeFailures, entry)
		}
	}
	return env
}

// applyRubricSpecs fills judge criteria's Definition/Scale/Bands from the node's YAML rubric specs by name.
// A raw planner rubric override has no specs (nil), leaving zero-value fields rather than failing the round.
func applyRubricSpecs(v verdict, specs map[string]criterionSpec) verdict {
	if len(specs) == 0 || len(v.Criteria) == 0 {
		return v
	}
	for name, c := range v.Criteria {
		if c.Deterministic {
			continue
		}
		if spec, ok := specs[name]; ok {
			c.Definition = spec.Definition
			c.Scale = spec.Scale
			c.Bands = spec.Bands
			v.Criteria[name] = c
		}
	}
	return v
}

// sanitizeAnchors drops (and logs) judge anchors that fail their check: quote not verbatim in the answer,
// or path outside the clone roots. An unlocatable complaint is not grounds to fail the round.
func sanitizeAnchors(v verdict, answer string, cfg Config) verdict {
	for name, c := range v.Criteria {
		if c.Anchor == nil {
			continue
		}
		if !validAnchor(c.Anchor, answer, cfg) {
			c.Anchor = nil
			v.Criteria[name] = c
		}
	}
	return v
}

func validAnchor(a *anchorSpec, answer string, cfg Config) bool {
	switch a.Kind {
	case "quote":
		if a.Text == "" || !strings.Contains(answer, a.Text) {
			slog.Warn("dropping quote anchor not found in answer", "component", "vetting", "text", a.Text)
			return false
		}
		return true
	case "path":
		if a.Path == "" {
			return false
		}
		if !pathUnderClone(a.Path, cfg) {
			slog.Warn("dropping path anchor outside clone roots", "component", "vetting", "path", a.Path)
			return false
		}
		return true
	case "omission":
		return a.Expected != "" // no span to check - absence is the point
	default:
		slog.Warn("dropping anchor with unknown kind", "component", "vetting", "kind", a.Kind)
		return false
	}
}

// pathUnderClone reports whether path resolves inside the node's clone root
// and exists there. No workspace wired ⇒ nothing to validate against ⇒ reject.
func pathUnderClone(path string, cfg Config) bool {
	if cfg.Workspace == nil {
		return false
	}
	abs, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, path)
	if err != nil {
		return false
	}
	_, err = os.Stat(abs)
	return err == nil
}
