// Package artifactschema validates an artifact kind's content against the
// JSON Schema an SDK extension declares for it (extsdk.ArtifactSchemas).
package artifactschema

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/fagerbergj/quack/internal/recordstore"
)

// Registry maps an artifact kind to its compiled schema. A nil *Registry is
// nil-safe and behaves as empty, so an unconfigured boot needs no special-casing.
type Registry struct {
	schemas map[string]*jsonschema.Resolved
	raw     map[string]json.RawMessage
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
	raw := map[string]json.RawMessage{}
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
			if err := checkKind(kind); err != nil {
				return nil, fmt.Errorf("artifact schema: extension %s: %w", source, err)
			}
			resolved, err := compile(bySource[source][kind])
			if err != nil {
				return nil, fmt.Errorf("artifact schema: extension %s: kind %q: %w", source, kind, err)
			}
			owner[kind] = source
			schemas[kind] = resolved
			raw[kind] = bySource[source][kind]
		}
	}
	if len(schemas) == 0 {
		return nil, nil
	}
	return &Registry{schemas: schemas, raw: raw}, nil
}

// checkKind rejects a kind no artifact-write path could ever produce (a
// typo) or one recordstore reserves for its own internal writes.
func checkKind(kind string) error {
	spec, ok := recordstore.SpecFor(kind)
	if !ok {
		return fmt.Errorf("kind %q is not a registered artifact kind", kind)
	}
	if spec.System || (spec.Class == recordstore.Structured && !spec.AgentWritable) {
		return fmt.Errorf("kind %q is written by quack itself and cannot carry an extension schema", kind)
	}
	// artifact_valid and the consuming page both look the artifact up by the
	// chat's hint id, which only a hint-identity kind is stored under.
	if !spec.RequiresHint {
		return fmt.Errorf("kind %q is not hint-identified, so its artifact cannot be found to validate", kind)
	}
	// text and bytes are where a refused or unstructured answer falls back to,
	// so a schema on either could lose a node's output outright.
	if kind == "text" || kind == "bytes" {
		return fmt.Errorf("kind %q is quack's fallback kind and cannot carry an extension schema", kind)
	}
	return nil
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

// FormatViolations renders "- path: problem" lines, one per violation -
// shared by every caller reporting a schema failure, so wording never drifts.
func FormatViolations(violations []string) string {
	lines := make([]string, len(violations))
	for i, v := range violations {
		lines[i] = "- " + v
	}
	return strings.Join(lines, "\n")
}

// FormatRefusal is the exact text a model sees when a write fails kind's
// schema - identical on the native and MCP tool surfaces.
func FormatRefusal(kind string, violations []string, schema json.RawMessage) string {
	msg := fmt.Sprintf("artifact not written: kind %q failed its schema:\n%s", kind, FormatViolations(violations))
	if len(schema) > 0 {
		// The validator stops at the first violation, so the schema itself lets one retry fix the rest.
		msg += fmt.Sprintf("\nThe schema this kind must satisfy:\n%s", schema)
	}
	return msg + "\nFix the content to match and call the tool again."
}

// Schema returns kind's registered schema document, nil when there is none.
func (r *Registry) Schema(kind string) json.RawMessage {
	if r == nil {
		return nil
	}
	return r.raw[kind]
}

// RefusalFromError renders a recordstore schema violation as the tool error
// every write surface returns, so the native and MCP paths cannot drift.
func RefusalFromError(err error) (string, bool) {
	var sv *recordstore.SchemaViolation
	if errors.As(err, &sv) {
		return FormatRefusal(sv.Kind, sv.Violations, sv.Schema), true
	}
	return "", false
}
