// Package artifactschema validates an artifact kind's content against the
// JSON Schema an SDK extension declares for it (extsdk.ArtifactSchemas).
package artifactschema

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// Registry maps an artifact kind to its compiled schema. A nil *Registry is
// nil-safe and behaves as empty, so an unconfigured boot needs no special-casing.
type Registry struct {
	schemas map[string]*jsonschema.Resolved
}

// Build compiles every schema the given extensions declare, keyed by
// extension name then kind. A duplicate kind or an uncompilable schema fails.
func Build(bySource map[string]map[string]json.RawMessage) (*Registry, error) {
	sourceNames := make([]string, 0, len(bySource))
	for name := range bySource {
		sourceNames = append(sourceNames, name)
	}
	sort.Strings(sourceNames)

	owner := map[string]string{}
	schemas := map[string]*jsonschema.Resolved{}
	for _, source := range sourceNames {
		kinds := make([]string, 0, len(bySource[source]))
		for kind := range bySource[source] {
			kinds = append(kinds, kind)
		}
		sort.Strings(kinds)
		for _, kind := range kinds {
			if other, dup := owner[kind]; dup {
				return nil, fmt.Errorf("artifact schema: kind %q registered by both %s and %s", kind, other, source)
			}
			resolved, err := compile(bySource[source][kind])
			if err != nil {
				return nil, fmt.Errorf("artifact schema: extension %s: kind %q: %w", source, kind, err)
			}
			owner[kind] = source
			schemas[kind] = resolved
		}
	}
	if len(schemas) == 0 {
		return nil, nil
	}
	return &Registry{schemas: schemas}, nil
}

func compile(raw json.RawMessage) (*jsonschema.Resolved, error) {
	var s jsonschema.Schema
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("invalid JSON Schema: %w", err)
	}
	resolved, err := s.Resolve(nil)
	if err != nil {
		return nil, fmt.Errorf("schema does not resolve: %w", err)
	}
	return resolved, nil
}

// Has reports whether kind has a registered schema.
func (r *Registry) Has(kind string) bool {
	if r == nil {
		return false
	}
	_, ok := r.schemas[kind]
	return ok
}

// Validate reports kind's schema violations against content ("path: problem"
// each). Nil means valid, or kind has no registered schema.
func (r *Registry) Validate(kind string, content []byte) []string {
	if r == nil {
		return nil
	}
	rs, ok := r.schemas[kind]
	if !ok {
		return nil
	}
	var instance any
	if err := json.Unmarshal(content, &instance); err != nil {
		return []string{fmt.Sprintf("(root): content is not valid JSON: %v", err)}
	}
	if err := rs.Validate(instance); err != nil {
		return []string{formatViolation(err)}
	}
	return nil
}

// validatingPrefixRe strips jsonschema-go's "validating <json-pointer>: "
// wrap chain so the deepest path survives as this violation's location.
var validatingPrefixRe = regexp.MustCompile(`^validating (\S+): `)

func formatViolation(err error) string {
	msg := err.Error()
	path := "(root)"
	for {
		m := validatingPrefixRe.FindStringSubmatch(msg)
		if m == nil {
			break
		}
		path = m[1]
		msg = msg[len(m[0]):]
	}
	return path + ": " + msg
}

// MaxViolationLines caps how many violations FormatRefusal lists before
// collapsing the rest into a count.
const MaxViolationLines = 12

// FormatViolations renders capped "- path: problem" lines - shared by every
// caller reporting a schema failure, so the wording never drifts.
func FormatViolations(violations []string) string {
	shown := violations
	var more int
	if len(shown) > MaxViolationLines {
		more = len(shown) - MaxViolationLines
		shown = shown[:MaxViolationLines]
	}
	lines := make([]string, 0, len(shown)+1)
	for _, v := range shown {
		lines = append(lines, "- "+v)
	}
	if more > 0 {
		lines = append(lines, fmt.Sprintf("...and %d more", more))
	}
	return strings.Join(lines, "\n")
}

// FormatRefusal is the exact text a model sees when a write fails kind's
// schema - identical on the native and MCP tool surfaces.
func FormatRefusal(kind string, violations []string) string {
	return fmt.Sprintf("artifact not written: kind %q failed its schema:\n%s\nFix these and call the tool again.",
		kind, FormatViolations(violations))
}
