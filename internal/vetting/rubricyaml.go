package vetting

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// rubricScale: a scale's bounds and pass floor, declared as data and validated at load time.
type rubricScale struct {
	Min  float64 `yaml:"min"`
	Max  float64 `yaml:"max"`
	Pass float64 `yaml:"pass"`
}

// rubricCriterion: one criterion's authoring content. Guidance/Steps are judge-only;
// Definition and Bands are shown to both judge and worker.
type rubricCriterion struct {
	Definition    string       `yaml:"definition"`
	Guidance      string       `yaml:"guidance,omitempty"`
	Steps         []string     `yaml:"steps,omitempty"`
	Bands         []bandSpec   `yaml:"bands"`
	Anchors       []string     `yaml:"anchors,omitempty"` // legal anchorSpec.Kind values; empty = any
	Deterministic bool         `yaml:"deterministic,omitempty"`
	Fix           string       `yaml:"fix,omitempty"` // required when Deterministic
	Scale         *rubricScale `yaml:"scale,omitempty"`
	// RequireFixOnFail: a below-threshold score with no `fix` is treated as self-inconsistent
	// and re-asked once (inconsistentJudgeFailures).
	RequireFixOnFail bool `yaml:"require_fix_on_fail,omitempty"`
}

// rubricDoc: a whole agents/<kind>/rubric.yaml. Notes is per-agent guidance that cuts across
// criteria; general grading rules live once, in the judge prompt.
type rubricDoc struct {
	Scale    rubricScale                `yaml:"scale"`
	Notes    string                     `yaml:"notes,omitempty"`
	Criteria map[string]rubricCriterion `yaml:"criteria"`
}

var validAnchorKinds = map[string]bool{"quote": true, "path": true, "omission": true}

// loadRubricYAML reads and validates one rubric.yaml. An invalid rubric FILE is an authoring bug,
// so it fails startup naming the criterion instead of falling back.
func loadRubricYAML(raw []byte, source string) (rubricDoc, error) {
	var doc rubricDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return rubricDoc{}, fmt.Errorf("vetting: parse rubric %q: %w", source, err)
	}
	if err := validateRubricDoc(doc); err != nil {
		return rubricDoc{}, fmt.Errorf("vetting: rubric %q: %w", source, err)
	}
	return doc, nil
}

// validateRubricDoc: bands cover the scale with no gaps or overlaps, the pass floor falls in a band,
// and a deterministic criterion declares a fix.
func validateRubricDoc(doc rubricDoc) error {
	if len(doc.Criteria) == 0 {
		return nil // a criteria-less rubric (e.g. memory-agent's prose guidance) has nothing to validate
	}
	if doc.Scale.Max <= doc.Scale.Min {
		return fmt.Errorf("top-level scale invalid: min=%v max=%v", doc.Scale.Min, doc.Scale.Max)
	}
	names := make([]string, 0, len(doc.Criteria))
	for name := range doc.Criteria {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := validateCriterion(name, doc.Criteria[name], doc.Scale); err != nil {
			return err
		}
	}
	return nil
}

// validateCriterion checks one criterion: its scale (criterion's own or the
// doc default), band coverage and pass containment, deterministic fix, anchors.
func validateCriterion(name string, c rubricCriterion, scale rubricScale) error {
	if c.Scale != nil {
		scale = *c.Scale
	}
	if scale.Max <= scale.Min {
		return fmt.Errorf("criterion %q: scale invalid: min=%v max=%v", name, scale.Min, scale.Max)
	}
	if scale.Pass <= scale.Min || scale.Pass > scale.Max {
		return fmt.Errorf("criterion %q: pass %v outside scale [%v, %v]", name, scale.Pass, scale.Min, scale.Max)
	}
	// bands:[] is legal only as an explicit choice, for a criterion whose source prose
	// didn't fit {min,max,meaning}.
	if len(c.Bands) > 0 {
		if err := validateBandCoverage(c.Bands, scale); err != nil {
			return fmt.Errorf("criterion %q: %w", name, err)
		}
		if !bandsContain(c.Bands, scale.Pass) {
			return fmt.Errorf("criterion %q: pass %v does not fall inside any band", name, scale.Pass)
		}
	}
	if c.Deterministic && strings.TrimSpace(c.Fix) == "" {
		return fmt.Errorf("criterion %q: deterministic criterion must declare fix", name)
	}
	for _, a := range c.Anchors {
		if !validAnchorKinds[a] {
			return fmt.Errorf("criterion %q: unknown anchor kind %q", name, a)
		}
	}
	return nil
}

// validateBandCoverage: sorted by Min, bands span exactly [scale.Min, scale.Max] with no overlap
// and no gap wider than 1 unit (0-3/4-6/7-10 counts as contiguous on an integer scale).
func validateBandCoverage(bands []bandSpec, scale rubricScale) error {
	sorted := append([]bandSpec(nil), bands...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Min < sorted[j].Min })
	const epsilon = 1e-9
	if sorted[0].Min > scale.Min+epsilon {
		return fmt.Errorf("bands start at %v, scale starts at %v", sorted[0].Min, scale.Min)
	}
	last := sorted[len(sorted)-1]
	if last.Max < scale.Max-epsilon {
		return fmt.Errorf("bands end at %v, scale ends at %v", last.Max, scale.Max)
	}
	for i := 1; i < len(sorted); i++ {
		prev, cur := sorted[i-1], sorted[i]
		if cur.Min < prev.Max-epsilon {
			return fmt.Errorf("bands overlap: [%v,%v] and [%v,%v]", prev.Min, prev.Max, cur.Min, cur.Max)
		}
		if cur.Min-prev.Max > 1+epsilon {
			return fmt.Errorf("gap between bands: [%v,%v] and [%v,%v]", prev.Min, prev.Max, cur.Min, cur.Max)
		}
	}
	return nil
}

func bandsContain(bands []bandSpec, v float64) bool {
	for _, b := range bands {
		if v >= b.Min && v <= b.Max {
			return true
		}
	}
	return false
}

// renderRubricMarkdown renders the per-criterion sections and Notes for the judge prompt;
// the general grading preamble lives in judge.go.
func renderRubricMarkdown(doc rubricDoc) string {
	var sb strings.Builder
	names := make([]string, 0, len(doc.Criteria))
	for name := range doc.Criteria {
		names = append(names, name)
	}
	sort.Strings(names)
	for i, name := range names {
		c := doc.Criteria[name]
		if i > 0 {
			sb.WriteString("\n---\n\n")
		}
		fmt.Fprintf(&sb, "### `%s`\n\n", name)
		sb.WriteString(strings.TrimSpace(c.Definition))
		sb.WriteString("\n")
		if c.Guidance != "" {
			sb.WriteString("\n")
			sb.WriteString(strings.TrimSpace(c.Guidance))
			sb.WriteString("\n")
		}
		if c.Deterministic {
			sb.WriteString("\nCode-owned: a deterministic check scores this criterion; the judge does not.\n")
			continue
		}
		if len(c.Steps) > 0 {
			sb.WriteString("\n**Evaluation steps.**\n")
			for i, step := range c.Steps {
				fmt.Fprintf(&sb, "%d. %s\n", i+1, step)
			}
		}
		if len(c.Bands) > 0 {
			sb.WriteString("\n**Scoring bands.**\n")
			bands := append([]bandSpec(nil), c.Bands...)
			sort.Slice(bands, func(i, j int) bool { return bands[i].Min > bands[j].Min }) // highest first
			for _, b := range bands {
				fmt.Fprintf(&sb, "- **%s** - %s\n", formatBandRange(b), b.Meaning)
			}
		}
	}
	if doc.Notes != "" {
		if sb.Len() > 0 {
			sb.WriteString("\n---\n\n")
		}
		sb.WriteString(strings.TrimSpace(doc.Notes))
		sb.WriteString("\n")
	}
	return strings.TrimSpace(sb.String())
}

// formatBandRange: "7–10" for an integer band, "0.75–1.00" for a fractional
// one - matches the source rubric.md's own formatting per criterion.
func formatBandRange(b bandSpec) string {
	if b.Min == float64(int64(b.Min)) && b.Max == float64(int64(b.Max)) {
		if b.Min == b.Max {
			return strconv.FormatInt(int64(b.Min), 10)
		}
		return fmt.Sprintf("%d–%d", int64(b.Min), int64(b.Max))
	}
	return fmt.Sprintf("%.2f–%.2f", b.Min, b.Max)
}

// rubricDocSpecs turns a loaded rubricDoc into the envelope's per-criterion specs.
func rubricDocSpecs(doc rubricDoc) map[string]criterionSpec {
	out := make(map[string]criterionSpec, len(doc.Criteria))
	for name, c := range doc.Criteria {
		scale := doc.Scale
		if c.Scale != nil {
			scale = *c.Scale
		}
		out[name] = criterionSpec{
			Name:             name,
			Definition:       c.Definition,
			Scale:            &scaleSpec{Min: scale.Min, Max: scale.Max},
			Bands:            c.Bands,
			RequireFixOnFail: c.RequireFixOnFail,
			Deterministic:    c.Deterministic,
		}
	}
	return out
}

// rubricDocPassMarks returns each criterion's own declared pass mark as a
// 0-1 fraction of its scale - informational only; the live gate ignores it.
func rubricDocPassMarks(doc rubricDoc) map[string]float64 {
	out := make(map[string]float64, len(doc.Criteria))
	for name, c := range doc.Criteria {
		scale := doc.Scale
		if c.Scale != nil {
			scale = *c.Scale
		}
		if scale.Max <= scale.Min {
			continue
		}
		out[name] = (scale.Pass - scale.Min) / (scale.Max - scale.Min)
	}
	return out
}

// rubricDocFixes: each deterministic criterion's declared fix; mergeDeterministic prefers it
// over its static fallback table.
func rubricDocFixes(doc rubricDoc) map[string]string {
	out := map[string]string{}
	for name, c := range doc.Criteria {
		if c.Deterministic && c.Fix != "" {
			out[name] = c.Fix
		}
	}
	return out
}
