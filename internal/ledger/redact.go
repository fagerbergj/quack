package ledger

import (
	"encoding/json"
	"strings"
)

// redactedKeys names JSON-object keys that must never reach the ledger.
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

// Redact walks a decoded JSON value and replaces credential-keyed values with a placeholder.
// Strings are probed as JSON too so pre-marshaled payloads don't bypass redaction.
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

// redactJSONString probes s as JSON; a parseable map/slice is redacted and re-marshaled.
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
