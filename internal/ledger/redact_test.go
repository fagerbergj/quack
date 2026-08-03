package ledger

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestRedact(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "top-level auth header",
			in:   `{"authorization":"Bearer sekret","gen_ai.tool.name":"web_fetch"}`,
			want: `{"authorization":"[REDACTED]","gen_ai.tool.name":"web_fetch"}`,
		},
		{
			name: "nested api key survives depth",
			in:   `{"gen_ai.tool.call.arguments":{"headers":{"x-api-key":"abc123"},"url":"https://x"}}`,
			want: `{"gen_ai.tool.call.arguments":{"headers":{"x-api-key":"[REDACTED]"},"url":"https://x"}}`,
		},
		{
			name: "key inside an array element",
			in:   `{"items":[{"token":"t1"},{"note":"keep me"}]}`,
			want: `{"items":[{"token":"[REDACTED]"},{"note":"keep me"}]}`,
		},
		{
			name: "case-insensitive match",
			in:   `{"Authorization":"x","API_KEY":"y"}`,
			want: `{"Authorization":"[REDACTED]","API_KEY":"[REDACTED]"}`,
		},
		{
			name: "no secret keys is a no-op",
			in:   `{"gen_ai.operation.name":"chat","quack.node":"n1"}`,
			want: `{"gen_ai.operation.name":"chat","quack.node":"n1"}`,
		},
		// Every emission seam (inference/tools/acp) hands the ledger complex
		// payloads pre-marshaled into ONE string attribute value (tool
		// arguments, ACP frames), not a structured tree - a secret buried in
		// that JSON-encoded STRING must still be caught.
		{
			name: "secret nested inside a JSON-encoded string attribute",
			in:   `{"gen_ai.tool.call.arguments":"{\"headers\":{\"authorization\":\"Bearer sekret\"},\"url\":\"https://x\"}"}`,
			want: `{"gen_ai.tool.call.arguments":"{\"headers\":{\"authorization\":\"[REDACTED]\"},\"url\":\"https://x\"}"}`,
		},
		{
			name: "a plain non-JSON string is left alone",
			in:   `{"gen_ai.tool.call.result":"not json, just prose about a token"}`,
			want: `{"gen_ai.tool.call.result":"not json, just prose about a token"}`,
		},
		{
			name: "a bare JSON scalar string is left alone",
			in:   `{"gen_ai.request.max_tokens":"256"}`,
			want: `{"gen_ai.request.max_tokens":"256"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var in any
			if err := json.Unmarshal([]byte(tt.in), &in); err != nil {
				t.Fatalf("bad test input: %v", err)
			}
			var want any
			if err := json.Unmarshal([]byte(tt.want), &want); err != nil {
				t.Fatalf("bad test want: %v", err)
			}
			got := Redact(in)
			if !reflect.DeepEqual(got, want) {
				gb, _ := json.Marshal(got)
				wb, _ := json.Marshal(want)
				t.Errorf("Redact(%s) = %s, want %s", tt.in, gb, wb)
			}
		})
	}
}
