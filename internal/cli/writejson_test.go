package cli

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

type writeJSONInner struct {
	Items []string `json:"items"`
}

type writeJSONFixture struct {
	Name     string                    `json:"name"`
	Tags     []string                  `json:"tags"`
	Nested   *writeJSONInner           `json:"nested"`
	Children []writeJSONInner          `json:"children"`
	ByKey    map[string]writeJSONInner `json:"by_key"`
	When     time.Time                 `json:"when"`
}

// TestWriteJSON_NestedNilSlices covers every place a nil slice can hide -
// pointer, slice element, map value - each must still encode `[]`.
func TestWriteJSON_NestedNilSlices(t *testing.T) {
	when := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	v := writeJSONFixture{
		Name:     "x",
		Tags:     nil,
		Nested:   &writeJSONInner{Items: nil},
		Children: []writeJSONInner{{Items: nil}, {Items: []string{"a"}}},
		ByKey:    map[string]writeJSONInner{"k": {Items: nil}},
		When:     when,
	}
	var out bytes.Buffer
	if err := WriteJSON(&out, v); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out.String())
	}
	cases := []struct {
		path string
		val  any
	}{
		{"tags", got["tags"]},
		{"nested.items", got["nested"].(map[string]any)["items"]},
		{"children[0].items", got["children"].([]any)[0].(map[string]any)["items"]},
		{"by_key.k.items", got["by_key"].(map[string]any)["k"].(map[string]any)["items"]},
	}
	for _, tc := range cases {
		arr, ok := tc.val.([]any)
		if !ok || arr == nil {
			t.Errorf("%s = %#v, want a non-nil empty array", tc.path, tc.val)
		}
	}

	// time.Time implements MarshalJSON via an unexported wall/ext pair -
	// denullSlices must leave it untouched, not zero it out.
	var roundTrip writeJSONFixture
	if err := json.Unmarshal(out.Bytes(), &roundTrip); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}
	if !roundTrip.When.Equal(when) {
		t.Errorf("when = %v, want %v (denullSlices must not touch time.Time's unexported fields)", roundTrip.When, when)
	}
}

// TestWriteJSON_RecoverSummaryReports: a nil slice two levels down a
// pointer-slice field must still encode `[]`.
func TestWriteJSON_RecoverSummaryReports(t *testing.T) {
	sum := &RecoverSummary{
		Chats:   1,
		Reports: []*LedgerRecoverReport{{ChatID: "c1"}},
	}
	var out bytes.Buffer
	if err := WriteJSON(&out, sum); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output not valid JSON: %v\n%s", err, out.String())
	}
	report := got["reports"].([]any)[0].(map[string]any)
	for _, field := range []string{"confirmed", "redone"} {
		arr, ok := report[field].([]any)
		if !ok || arr == nil {
			t.Errorf("reports[0].%s = %#v, want a non-nil empty array", field, report[field])
		}
	}
}
