package vetting

import (
	"encoding/json"
	"log/slog"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// marshalEnvelope renders the envelope as indented JSON for the worker's
// revise prompt fenced block. Falls back to a terse error string rather than
// panicking - a malformed envelope must not crash the revise round.
func marshalEnvelope(env verdictEnvelope) string {
	b, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return `{"error": "failed to render verdict envelope"}`
	}
	return string(b)
}

// scaleSpec: a criterion's score range, explicit per criterion (#941) - judge
// criteria are 0-10, deterministic checks like cites_sources keep their own
// native scale (0-1).
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
}

// anchorSpec: where in the answer a criticism points. Typed per #941; kind
// determines which of the other fields apply.
type anchorSpec struct {
	Kind string `json:"kind"` // quote | path | omission

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

// failureEntry: one failed criterion. Deterministic and judge failures are
// the same type; they differ only in which optional fields are set
// (Evidence for deterministic, Anchor for judge).
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

// verdictEnvelope: the structured replacement for composeFeedback's prose (#941).
type verdictEnvelope struct {
	Passed    bool    `json:"passed"`
	Score     float64 `json:"score"`
	Threshold float64 `json:"threshold"`
	// Scoring is currently always "lowest_criterion" - weakest-link gating (#941 non-goal: no change to scoring).
	Scoring               string         `json:"scoring"`
	Round                 int            `json:"round"`
	DeterministicFailures []failureEntry `json:"deterministic_failures,omitempty"`
	JudgeFailures         []failureEntry `json:"judge_failures,omitempty"`
	Passing               []passingEntry `json:"passing,omitempty"`
}

const scoringLowestCriterion = "lowest_criterion"

// criterionText: a criterion's diagnosis text, preferring the new Shortfall
// field over the deprecated Reason - aggregateVerdict only copies Reason INTO
// Shortfall, never the reverse, so a judge that submits only `shortfall`
// (the new schema) must still be readable everywhere that used to read `reason`.
func criterionText(c criterionScore) string {
	if s := strings.TrimSpace(c.Shortfall); s != "" {
		return c.Shortfall
	}
	return c.Reason
}

// buildEnvelope replaces composeFeedback's prose rendering: it turns a
// verdict into the structured shape #941 specifies. Deterministic failures
// are named by mergeDeterministic (Deterministic==true); everything else
// failing below threshold is a judge failure.
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
			Shortfall: c.Shortfall,
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

// ── Rubric parsing (#941) ──────────────────────────────────────────────

// rubricSectionRe matches a "### `name`" criterion header.
var rubricSectionRe = regexp.MustCompile("(?m)^### `([a-zA-Z0-9_]+)`\\s*$")

// rubricBandRe matches a scoring-band bullet: "- **N** - meaning" or "- **N–M** - meaning" (en dash).
var rubricBandRe = regexp.MustCompile(`^-\s+\*\*(\d+)(?:[–-](\d+))?\*\*\s*-\s*(.+)$`)

const evaluationStepsMarker = "**Evaluation steps.**"
const scoringBandsMarker = "**Scoring bands.**"

// judgeRubricScale: every prose rubric.md in agents/*/ scores 0-10 integers (per rubric preamble).
var judgeRubricScale = &scaleSpec{Min: 0, Max: 10}

// parseRubric extracts each "### `name`" section's definition and scoring
// bands from a rubric.md. A section that doesn't parse (bands missing/malformed)
// falls back to its raw text as definition with empty bands - rubric formatting
// never fails the round (#941).
func parseRubric(rubric string) map[string]criterionSpec {
	out := map[string]criterionSpec{}
	matches := rubricSectionRe.FindAllStringSubmatchIndex(rubric, -1)
	for i, m := range matches {
		name := rubric[m[2]:m[3]]
		start := m[1]
		end := len(rubric)
		if i+1 < len(matches) {
			end = matches[i+1][0]
		}
		section := strings.TrimSpace(rubric[start:end])
		out[name] = parseRubricSection(name, section)
	}
	return out
}

// parseRubricSection parses one criterion's section body (header line already stripped).
func parseRubricSection(name, section string) criterionSpec {
	definition := section
	if idx := strings.Index(section, evaluationStepsMarker); idx >= 0 {
		definition = strings.TrimSpace(section[:idx])
	}
	bands := parseRubricBands(section)
	if len(bands) == 0 {
		// Fallback: unparseable section -> raw text in definition, empty bands (#941).
		return criterionSpec{Name: name, Definition: section, Scale: judgeRubricScale}
	}
	return criterionSpec{Name: name, Definition: definition, Scale: judgeRubricScale, Bands: bands}
}

// parseRubricBands parses the "**Scoring bands.**" bullet list out of a criterion section.
func parseRubricBands(section string) []bandSpec {
	idx := strings.Index(section, scoringBandsMarker)
	if idx < 0 {
		return nil
	}
	body := section[idx+len(scoringBandsMarker):]
	// Bands run to the end of the section (sections are already split at the next header).
	if cut := strings.Index(body, "\n---"); cut >= 0 {
		body = body[:cut]
	}
	var bands []bandSpec
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		mm := rubricBandRe.FindStringSubmatch(line)
		if mm == nil {
			continue
		}
		lo, err := strconv.ParseFloat(mm[1], 64)
		if err != nil {
			continue
		}
		hi := lo
		if mm[2] != "" {
			hi, err = strconv.ParseFloat(mm[2], 64)
			if err != nil {
				continue
			}
		}
		bands = append(bands, bandSpec{Min: lo, Max: hi, Meaning: strings.TrimSpace(mm[3])})
	}
	return bands
}

// applyRubricSpecs fills each judge (non-deterministic) failing/passing
// criterion's Definition/Scale/Bands from the parsed rubric, by name. Missing
// from the rubric entirely (should not happen for a real judge criterion) is
// left with zero-value spec fields rather than failing the round.
func applyRubricSpecs(v verdict, rubric string) verdict {
	if rubric == "" || len(v.Criteria) == 0 {
		return v
	}
	specs := parseRubric(rubric)
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

// ── Anchor enforcement (#941) ───────────────────────────────────────────

// sanitizeAnchors drops any judge-submitted anchor that fails its gate check
// (quote not found verbatim in the answer, or path outside the node's clone
// roots), logging each drop - the judge invented a locatable complaint that
// isn't actually locatable, not grounds to fail the round.
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
