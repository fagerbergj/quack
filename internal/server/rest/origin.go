package rest

import (
	"encoding/json"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"

	"github.com/fagerbergj/quack/internal/schema"
)

// chatOrigin decodes a chat row's opaque Origin JSON (marshaled from
// *extsdk.ChatOrigin by an extension's Dispatch - see
// internal/serve/extensions.go's newExtDispatch) into the wire schema. Nil
// on no origin, a malformed blob, or a missing required field - never a
// partially-filled chip.
func chatOrigin(originJSON string) *schema.ChatOrigin {
	if originJSON == "" {
		return nil
	}
	var sdkOrigin extsdk.ChatOrigin
	if err := json.Unmarshal([]byte(originJSON), &sdkOrigin); err != nil {
		return nil
	}
	if sdkOrigin.Extension == "" || sdkOrigin.Label == "" {
		return nil
	}
	out := schema.ChatOrigin{
		Extension: sdkOrigin.Extension,
		Label:     sdkOrigin.Label,
		Kind:      strPtr(sdkOrigin.Kind),
		Href:      strPtr(sdkOrigin.Href),
		Badge:     strPtr(sdkOrigin.Badge),
	}
	if len(sdkOrigin.Labels) > 0 {
		// Labels' generated element type is an anonymous struct (oapi-codegen);
		// go through wireLabelValue (omitempty-tagged) rather than hand-matching
		// its field order, and so an unset Display/Href lands as absent, not a
		// pointer to "".
		wire := make(map[string][]wireLabelValue, len(sdkOrigin.Labels))
		for dim, vals := range sdkOrigin.Labels {
			values := make([]wireLabelValue, len(vals))
			for i, v := range vals {
				values[i] = wireLabelValue{Value: v.Value, Display: v.Display, Href: v.Href}
			}
			wire[dim] = values
		}
		if b, err := json.Marshal(wire); err == nil {
			_ = json.Unmarshal(b, &out.Labels)
		}
	}
	return &out
}

// wireLabelValue mirrors the openapi ChatOrigin.labels element schema's
// JSON shape (value/display,omitempty/href,omitempty) - the bridge onto the
// generated anonymous struct type via a JSON round-trip.
type wireLabelValue struct {
	Value   string `json:"value"`
	Display string `json:"display,omitempty"`
	Href    string `json:"href,omitempty"`
}
