package ledger

import (
	"encoding/json"
	"strings"
)

// redactedKeys names JSON-object keys whose value must never reach the
// ledger, however deep in the payload they appear - auth headers, API keys,
// endpoint credentials. Not configurable: Redact runs unconditionally before
// every write.
var redactedKeys = map[string]bool{
	"authorization": true, "www-authenticate": true, "proxy-authorization": true,
	"api_key": true, "apikey": true, "api-key": true, "x-api-key": true,
	"token": true, "access_token": true, "refresh_token": true, "id_token": true,
	"secret": true, "client_secret": true, "webhook_secret": true,
	"password": true, "passwd": true,
	"private_key": true, "privatekey": true,
	"cookie": true, "set-cookie": true,
}

const redactedValue = "[REDACTED]"

// Redact walks a decoded JSON value (the map[string]any/[]any/scalar shape
// json.Unmarshal produces) and replaces every value keyed by a credential
// name with a fixed placeholder, recursively. Keys match case-insensitively;
// scalars and unrecognized keys pass through unchanged.
//
// A string is also probed as JSON: emission seams (inference/tools/acp) hand
// the ledger complex payloads pre-marshaled into ONE string attribute, not a
// structured log.Value tree, so a credential inside one of those blobs would
// otherwise sail past the map/slice cases below untouched. A string that
// fails to parse passes through unchanged - never re-marshaled, so it can't
// pick up incidental round-trip formatting differences.
func Redact(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if redactedKeys[strings.ToLower(k)] {
				out[k] = redactedValue
				continue
			}
			out[k] = Redact(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = Redact(e)
		}
		return out
	case string:
		return redactJSONString(x)
	default:
		return v
	}
}

// redactJSONString probes s as JSON; a parse that yields a map or slice is
// redacted and re-marshaled back to a string (the caller's attribute stays a
// string either way - only its content changes). Anything else (parse
// failure, or valid JSON that's just a bare scalar) returns s unchanged.
func redactJSONString(s string) string {
	var decoded any
	if err := json.Unmarshal([]byte(s), &decoded); err != nil {
		return s
	}
	switch decoded.(type) {
	case map[string]any, []any:
	default:
		return s // a bare JSON scalar ("42", "true") - nothing to redact
	}
	b, err := json.Marshal(Redact(decoded))
	if err != nil {
		return s
	}
	return string(b)
}
