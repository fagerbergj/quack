package vetting

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// rubricScale: a scale's bounds and pass floor. Per #941 redirect: the
// rubric IS data now (the envelope already carries definition/scale/bands),
// so the scale that used to live only in prose ("7 is the lowest passing
// score") is declared here and validated at load time instead of eyeballed.
type rubricScale struct {
	Min  float64 `yaml:"min"`
	Max  float64 `yaml:"max"`
	Pass float64 `yaml:"pass"`
}

// rubricCriterion: one criterion's full authoring content. Guidance/Steps are
// judge-facing only (never shown to the worker in the envelope); Definition
// and Bands are shown to both judge and worker.
type rubricCriterion struct {
	Definition    string       `yaml:"definition"`
	Guidance      string       `yaml:"guidance,omitempty"`
	Steps         []string     `yaml:"steps,omitempty"`
	Bands         []bandSpec   `yaml:"bands"`
	Anchors       []string     `yaml:"anchors,omitempty"` // legal anchorSpec.Kind values for this criterion; empty = unrestricted
	Deterministic bool         `yaml:"deterministic,omitempty"`
	Fix           string       `yaml:"fix,omitempty"` // required when Deterministic
	Scale         *rubricScale `yaml:"scale,omitempty"`
}

// rubricDoc: a whole agents/<kind>/rubric.yaml. Notes is per-agent domain
// guidance that cuts across criteria (e.g. web-researcher's date-awareness
// and zero-retrieval handling) - NOT a restatement of how to grade in
// general, which lives in the judge prompt (judge.go) so it exists once.
type rubricDoc struct {
	Scale    rubricScale                `yaml:"scale"`
	Notes    string                     `yaml:"notes,omitempty"`
	Criteria map[string]rubricCriterion `yaml:"criteria"`
}

var validAnchorKinds = map[string]bool{"quote": true, "path": true, "omission": true}

// loadRubricYAML reads and validates one rubric.yaml. Validation failure is a
// startup error naming the criterion - never a silent fallback (#941 redirect):
// unlike the worker-facing rubric-parsing fallback (still used for a raw,
// unstructured planner-authored rubric override, see applyRubricSpecs), a
// rubric FILE that doesn't validate is an authoring bug, not a formatting
// quirk to tolerate.
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

// validateRubricDoc checks: every criterion's bands cover its scale with no
// gaps or overlaps, its scale's pass floor falls inside some band, and a
// deterministic criterion declares a fix.
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
		c := doc.Criteria[name]
		scale := doc.Scale
		if c.Scale != nil {
			scale = *c.Scale
		}
		if scale.Max <= scale.Min {
			return fmt.Errorf("criterion %q: scale invalid: min=%v max=%v", name, scale.Min, scale.Max)
		}
		if scale.Pass <= scale.Min || scale.Pass > scale.Max {
			return fmt.Errorf("criterion %q: pass %v outside scale [%v, %v]", name, scale.Pass, scale.Min, scale.Max)
		}
		// bands:[] is legal but must be an explicit authoring choice, not a
		// silently-tolerated omission - only for a criterion whose source
		// prose genuinely didn't fit {min,max,meaning} (documented in the PR;
		// see code-implementer's claims_match_activity and code-reviewer's
		// claims_grounded, whose original 0-2/4-6/7-10 bands skip 3 with no
		// stated reason - a real gap in the source, not a conversion bug).
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
	}
	return nil
}

// validateBandCoverage: sorted by Min, bands must span exactly [scale.Min,
// scale.Max] with no gap wider than 1 unit (an integer scale's adjacent
// bands, e.g. 0-3/4-6/7-10, are contiguous in that sense though not
// touching) and no overlap.
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

// renderRubricMarkdown renders a rubricDoc's criteria (and notes) as the
// markdown the judge prompt carries - the "how to grade" preamble (0-3
// scale walkthrough, weakest-link aggregation) is judge-prompt content now
// (judge.go), not rendered here, so this covers only the per-criterion
// sections plus any cross-cutting Notes.
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
		if len(c.Steps) > 0 {
			sb.WriteString("\n**Evaluation steps.**\n")
			for i, step := range c.Steps {
				fmt.Fprintf(&sb, "%d. %s\n", i+1, step)
			}
		}
		if len(c.Bands) > 0 {
			sb.WriteString("\n**Scoring bands.**\n")
			bands := append([]bandSpec(nil), c.Bands...)
			sort.Slice(bands, func(i, j int) bool { return bands[i].Min > bands[j].Min }) // highest first, matches rubric.md convention
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

// rubricDocSpecs turns a loaded rubricDoc directly into the envelope's
// per-criterion specs - no parsing back out of rendered markdown (#941
// redirect: the rubric IS data, so this is a lookup, not a parser).
func rubricDocSpecs(doc rubricDoc) map[string]criterionSpec {
	out := make(map[string]criterionSpec, len(doc.Criteria))
	for name, c := range doc.Criteria {
		scale := doc.Scale
		if c.Scale != nil {
			scale = *c.Scale
		}
		out[name] = criterionSpec{
			Name:       name,
			Definition: c.Definition,
			Scale:      &scaleSpec{Min: scale.Min, Max: scale.Max},
			Bands:      c.Bands,
		}
	}
	return out
}

// rubricDocFixes returns the declared fix text for each deterministic
// criterion in doc - mergeDeterministic (node.go) prefers this over its
// static fallback table when the rubric names the criterion (#941 redirect:
// "deterministic checks now read definition/bands/fix from the rubric entry
// instead of declaring them in Go").
func rubricDocFixes(doc rubricDoc) map[string]string {
	out := map[string]string{}
	for name, c := range doc.Criteria {
		if c.Deterministic && c.Fix != "" {
			out[name] = c.Fix
		}
	}
	return out
}
