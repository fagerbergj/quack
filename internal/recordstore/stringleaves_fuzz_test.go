package recordstore

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

// refStringValues is an independent, recursive-descent reference walk over
// the same token stream stringLeaves consumes. It never collapses duplicate
// object keys the way decoding into map[string]any would, so it stays a
// fair oracle even on JSON that (however unlikely from our own Marshal)
// repeats a key.
func refStringValues(t *testing.T, content []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(content))
	tok, err := dec.Token()
	if err != nil {
		t.Fatalf("refStringValues: %v", err)
	}
	return refStringValuesTok(t, dec, tok)
}

func refStringValuesTok(t *testing.T, dec *json.Decoder, tok json.Token) []string {
	t.Helper()
	switch v := tok.(type) {
	case string:
		return []string{v}
	case json.Delim:
		var out []string
		switch v {
		case '{':
			for {
				kt, err := dec.Token()
				if err != nil {
					t.Fatalf("refStringValues: %v", err)
				}
				if d, ok := kt.(json.Delim); ok && d == '}' {
					return out
				}
				vt, err := dec.Token()
				if err != nil {
					t.Fatalf("refStringValues: %v", err)
				}
				out = append(out, refStringValuesTok(t, dec, vt)...)
			}
		case '[':
			for {
				vt, err := dec.Token()
				if err != nil {
					t.Fatalf("refStringValues: %v", err)
				}
				if d, ok := vt.(json.Delim); ok && d == ']' {
					return out
				}
				out = append(out, refStringValuesTok(t, dec, vt)...)
			}
		}
	}
	return nil // number, bool, null
}

// FuzzApplyStructuredEdits proves stringLeaves' byte-offset bookkeeping and
// applyStructuredEdits' splice against every JSON string edge case: escaped
// quotes/backslashes, unicode and surrogate-pair escapes, raw non-ASCII,
// strings containing JSON syntax characters, empty strings, keys shaped
// like values, nested arrays, scalars adjacent to strings, and pretty vs
// compact spacing.
func FuzzApplyStructuredEdits(f *testing.F) {
	seeds := []struct{ content, newVal string }{
		{`{"a":"say \"hi\" now"}`, "ok"},
		{`{"a":"trailing backslash a\\"}`, "x"},
		{`{"a":"café"}`, "b"},
		{`{"a":"😀 emoji"}`, "b"},
		{`{"a":"café raw utf8"}`, "b"},
		{`{"a":"has { [ : , chars in it"}`, "y"},
		{`{"a":""}`, "was empty"},
		{`{"a":["x","y","x2"]}`, "z"},
		{`{"a":1,"b":"val","c":true,"d":null}`, "w"},
		{"{\n  \"a\": \"pretty printed\"\n}", "compact-now"},
		{`{"a":"<script>&</script>"}`, "<b>&amp;</b>"},
		{`{"key looks like value":"val"}`, "z"},
		{`{"a":{"b":{"c":"nested"}}}`, "deep"},
		{`["top","level","array"]`, "changed"},
	}
	for _, s := range seeds {
		f.Add(s.content, s.newVal)
	}
	f.Fuzz(func(t *testing.T, content, newVal string) {
		raw := []byte(content)
		var probe any
		if err := json.Unmarshal(raw, &probe); err != nil {
			return // not a single well-formed JSON document - Edit never sees this
		}
		if !utf8.ValidString(newVal) {
			return // json.Marshal lossily replaces invalid UTF-8 with U+FFFD - not this code's concern
		}

		leaves, err := stringLeaves(raw)
		if err != nil {
			t.Fatalf("stringLeaves failed on json.Valid input %q: %v", raw, err)
		}

		// Invariant: each leaf's byte span is exactly its JSON string
		// literal, decoding to the leaf's recorded value.
		for _, l := range leaves {
			if l.start < 0 || l.end > len(raw) || l.start >= l.end {
				t.Fatalf("leaf span [%d:%d] out of bounds (len=%d) in %q", l.start, l.end, len(raw), raw)
			}
			var got string
			if err := json.Unmarshal(raw[l.start:l.end], &got); err != nil {
				t.Fatalf("leaf span %q is not a JSON string literal: %v", raw[l.start:l.end], err)
			}
			if got != l.value {
				t.Fatalf("leaf span decodes to %q, want %q", got, l.value)
			}
		}

		// Invariant: leaves are exactly the value-position strings (never
		// keys, never a key+value concatenation), matching an independent walk.
		want := refStringValues(t, raw)
		gotVals := make([]string, 0, len(leaves))
		for _, l := range leaves {
			gotVals = append(gotVals, l.value)
		}
		sort.Strings(want)
		sort.Strings(gotVals)
		if len(want) != 0 || len(gotVals) != 0 {
			if !reflect.DeepEqual(want, gotVals) {
				t.Fatalf("stringLeaves values = %v, want %v (input %q)", gotVals, want, raw)
			}
		}

		// Exercise the splice on every leaf whose value is an unambiguous
		// match (mirrors applyStructuredEdits' own uniqueness precondition).
		for _, l := range leaves {
			if l.value == "" {
				continue // strings.Count("", "") edge case, covered separately
			}
			total := 0
			for _, m := range leaves {
				total += strings.Count(m.value, l.value)
			}
			if total != 1 {
				continue
			}
			out, err := applyStructuredEdits(raw, []EditOp{{Old: l.value, New: newVal}})
			if err != nil {
				t.Fatalf("applyStructuredEdits(%q -> %q) on %q: %v", l.value, newVal, raw, err)
			}
			if !json.Valid(out) {
				t.Fatalf("edit result is not valid JSON: %s", out)
			}
			if !bytes.Equal(out[:l.start], raw[:l.start]) {
				t.Fatalf("bytes before edited leaf changed: got %q, want %q", out[:l.start], raw[:l.start])
			}
			suffix := raw[l.end:]
			if !bytes.Equal(out[len(out)-len(suffix):], suffix) {
				t.Fatalf("bytes after edited leaf changed: got %q, want %q", out[len(out)-len(suffix):], suffix)
			}
			outLeaves, err := stringLeaves(out)
			if err != nil {
				t.Fatalf("stringLeaves on edit result: %v", err)
			}
			if len(outLeaves) != len(leaves) {
				t.Fatalf("leaf count changed: %d -> %d", len(leaves), len(outLeaves))
			}
			diffs := 0
			for i := range leaves {
				if leaves[i].value != outLeaves[i].value {
					diffs++
					if outLeaves[i].value != newVal {
						t.Fatalf("changed leaf = %q, want %q", outLeaves[i].value, newVal)
					}
				}
			}
			// A true no-op (New == Old) legitimately produces 0 semantic
			// diffs; any real edit must change exactly the matched leaf.
			wantDiffs := 1
			if newVal == l.value {
				wantDiffs = 0
			}
			if diffs != wantDiffs {
				t.Fatalf("%d leaves changed, want exactly %d (input %q)", diffs, wantDiffs, raw)
			}
			break // one exercised edit per input is enough
		}
	})
}
