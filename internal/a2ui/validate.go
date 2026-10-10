package a2ui

import (
	"embed"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
)

//go:embed schema/*.json
var schemaFS embed.FS

const (
	specBase   = "https://a2ui.org/specification/v0_9/"
	specVer    = "v0.9.1"
	maxErrText = 600
)

type schemas struct {
	message    *jsonschema.Resolved
	components map[string]*jsonschema.Resolved
	allowed    map[string][]string // component name -> every property its allOf branches declare
	names      []string
}

var loadSchemas = sync.OnceValues(func() (*schemas, error) {
	docs := map[string][]byte{}
	parsed := map[string]map[string]any{}
	for _, name := range []string{"server_to_client.json", "common_types.json", "catalog.json"} {
		b, err := schemaFS.ReadFile("schema/" + name)
		if err != nil {
			return nil, err
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, fmt.Errorf("a2ui: %s: %w", name, err)
		}
		docs[specBase+name], parsed[name] = b, m
	}
	names, err := flattenCatalog(parsed["catalog.json"])
	if err != nil {
		return nil, err
	}
	if docs[specBase+"catalog.json"], err = flattenedJSON(parsed["catalog.json"]); err != nil {
		return nil, err
	}
	// Fresh unmarshal per load: a resolver keys its bookkeeping by *Schema.
	opts := &jsonschema.ResolveOptions{Loader: func(u *url.URL) (*jsonschema.Schema, error) {
		b, ok := docs[u.String()]
		if !ok {
			return nil, fmt.Errorf("a2ui: no embedded schema %s", u)
		}
		var s jsonschema.Schema
		return &s, json.Unmarshal(b, &s)
	}}
	resolve := func(ref string) (*jsonschema.Resolved, error) {
		return (&jsonschema.Schema{Schema: "https://json-schema.org/draft/2020-12/schema", Ref: ref}).Resolve(opts)
	}
	out := &schemas{components: map[string]*jsonschema.Resolved{}, allowed: map[string][]string{}, names: names}
	if out.message, err = resolve(specBase + "server_to_client.json"); err != nil {
		return nil, fmt.Errorf("a2ui: resolve server_to_client: %w", err)
	}
	defs := mergedDefs(parsed["common_types.json"], parsed["catalog.json"])
	for _, n := range names {
		if out.components[n], err = resolve(specBase + "catalog.json#/$defs/" + n); err != nil {
			return nil, fmt.Errorf("a2ui: resolve component %s: %w", n, err)
		}
		out.allowed[n] = allowedProps(defs, defs[n])
	}
	return out, nil
})

// flattenCatalog moves the catalog's components/functions into $defs (jsonschema-go follows pointers only through
// schema keywords) and sets the $id its relative refs resolve to. Returns the component names.
func flattenCatalog(cat map[string]any) ([]string, error) {
	defs, _ := cat["$defs"].(map[string]any)
	comps, _ := cat["components"].(map[string]any)
	fns, _ := cat["functions"].(map[string]any)
	for _, m := range []map[string]any{comps, fns} {
		for k, v := range m {
			if _, clash := defs[k]; clash {
				return nil, fmt.Errorf("a2ui: catalog entry %q collides with a $defs name", k)
			}
			defs[k] = v
		}
	}
	delete(cat, "components")
	delete(cat, "functions")
	cat["$id"] = specBase + "catalog.json"
	return slices.Sorted(maps.Keys(comps)), nil
}

func flattenedJSON(cat map[string]any) ([]byte, error) {
	b, err := json.Marshal(cat)
	if err != nil {
		return nil, err
	}
	return []byte(strings.NewReplacer(`"#/components/`, `"#/$defs/`, `"#/functions/`, `"#/$defs/`).Replace(string(b))), nil
}

func mergedDefs(docs ...map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, d := range docs {
		defs, _ := d["$defs"].(map[string]any)
		for k, v := range defs {
			if m, ok := v.(map[string]any); ok {
				out[k] = m
			}
		}
	}
	return out
}

// allowedProps: the property names across comp's allOf branches, following
// one level of $ref (ComponentCommon, CatalogComponentCommon, Checkable).
func allowedProps(defs map[string]map[string]any, comp map[string]any) []string {
	set := map[string]bool{}
	branches, _ := comp["allOf"].([]any)
	for _, b := range branches {
		m, _ := b.(map[string]any)
		if ref, ok := m["$ref"].(string); ok {
			m = defs[ref[strings.LastIndex(ref, "/")+1:]]
		}
		props, _ := m["properties"].(map[string]any)
		for k := range props {
			set[k] = true
		}
	}
	return slices.Sorted(maps.Keys(set))
}

// Validate checks s, wrapped in the envelope a client receives, against the v0.9.1 schema, quack's catalog,
// and component integrity. The error names the first problem.
func Validate(s Surface) error {
	if err := CheckSurfaceID(s.SurfaceID); err != nil {
		return err
	}
	if s.CatalogID != CatalogID {
		return fmt.Errorf("catalog_id %q is not %q", s.CatalogID, CatalogID)
	}
	sc, err := loadSchemas()
	if err != nil {
		return err
	}
	var inst []any // JSON-shaped values, so the validator never sees Go-specific types
	if err := roundTrip(s.Components, &inst); err != nil {
		return err
	}
	for i, c := range inst {
		if err := sc.component(i, c); err != nil {
			return err
		}
	}
	if err := checkIntegrity(s.Components); err != nil {
		return err
	}
	msgs := []map[string]any{
		{"version": specVer, "createSurface": map[string]any{"surfaceId": s.SurfaceID, "catalogId": s.CatalogID}},
		{"version": specVer, "updateComponents": map[string]any{"surfaceId": s.SurfaceID, "components": inst}},
	}
	if s.DataModel != nil {
		msgs = append(msgs, map[string]any{"version": specVer, "updateDataModel": map[string]any{"surfaceId": s.SurfaceID, "path": "/", "value": s.DataModel}})
	}
	for _, m := range msgs {
		var v any
		if err := roundTrip(m, &v); err != nil {
			return err
		}
		if err := sc.message.Validate(v); err != nil {
			return fmt.Errorf("envelope: %s", explain(err))
		}
	}
	return nil
}

// component validates one component against its own catalog entry, which
// names the offending property instead of a bare anyComponent oneOf failure.
func (sc *schemas) component(i int, v any) error {
	c, _ := v.(map[string]any)
	id, _ := c["id"].(string)
	name, _ := c["component"].(string)
	rs, ok := sc.components[name]
	if !ok {
		return fmt.Errorf("components[%d] %q: unknown component %q (catalog has %s)", i, id, name, strings.Join(sc.names, ", "))
	}
	err := rs.Validate(v)
	if err == nil {
		return nil
	}
	msg := explain(err)
	if strings.Contains(err.Error(), "/unevaluatedProperties") {
		var unknown []string
		for k := range c {
			if !slices.Contains(sc.allowed[name], k) {
				unknown = append(unknown, k)
			}
		}
		slices.Sort(unknown)
		msg = fmt.Sprintf("unknown properties %q (%s allows: %s)", unknown, name, strings.Join(sc.allowed[name], ", "))
	}
	return fmt.Errorf("components[%d] %q (%s): %s", i, id, name, msg)
}

func roundTrip(in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

var (
	propRe    = regexp.MustCompile(`properties/([^/:\s]+)`)
	defRe     = regexp.MustCompile(`\$defs/([^/:\s]+)`)
	schemaSet = regexp.MustCompile(` (of|against) \[?<anonymous schema>.*$`)
)

// shapeHints spells out the common_types unions whose oneOf failure otherwise says nothing actionable.
var shapeHints = map[string]string{
	"DynamicString":     `a string, {"path": "/json/pointer"}, or {"call": "fn", "args": {...}, "returnType": "string"}`,
	"DynamicNumber":     `a number, {"path": "/json/pointer"}, or {"call": "fn", "args": {...}, "returnType": "number"}`,
	"DynamicBoolean":    `true/false, {"path": "/json/pointer"}, or {"call": "fn", "args": {...}, "returnType": "boolean"}`,
	"DynamicStringList": `an array of strings, {"path": "/json/pointer"}, or {"call": "fn", "args": {...}, "returnType": "array"}`,
	"DynamicValue":      `a string, number, boolean, array, {"path": "/json/pointer"}, or a function call`,
	"DataBinding":       `{"path": "/json/pointer"} with no other keys`,
	"ChildList":         `an array of component id strings, or {"componentId": "id", "path": "/list/pointer"}`,
	"Action":            `{"event": {"name": "submit_quiz", "context": {"key": {"path": "/pointer"}}}} or {"functionCall": {"call": "openUrl", "args": {"url": "https://..."}}}`,
	"FunctionCall":      `{"call": "<catalog function>", "args": {...}} - one of: required, regex, length, numeric, email, formatString, formatNumber, formatCurrency, formatDate, pluralize, openUrl, and, or, not`,
	"anyFunction":       `a catalog function call: {"call": "<name>", "args": {...}} with that function's args`,
}

// explain turns jsonschema-go's "validating <pointer>: " chain into the
// innermost property, the $defs type it failed, and the keyword message.
func explain(err error) string {
	full := err.Error()
	msg := full
	if i := strings.LastIndex(msg, "validating "); i >= 0 {
		if j := strings.Index(msg[i:], ": "); j >= 0 {
			msg = msg[i+j+2:]
		}
	}
	msg = schemaSet.ReplaceAllString(msg, "")
	if m := defRe.FindAllStringSubmatch(full, -1); len(m) > 0 {
		def := m[len(m)-1][1]
		if hint, ok := shapeHints[def]; ok && strings.HasPrefix(msg, "oneOf") {
			msg = fmt.Sprintf("must be %s (%s)", hint, def)
		} else {
			msg = fmt.Sprintf("does not match %s: %s", def, msg)
		}
	}
	if m := propRe.FindAllStringSubmatch(full, -1); len(m) > 0 {
		msg = fmt.Sprintf("property %q %s", m[len(m)-1][1], msg)
	}
	return truncate(msg, maxErrText)
}

// truncate cuts s to at most n bytes without splitting a UTF-8 rune.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "..."
}
