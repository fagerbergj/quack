package rest

import (
	"encoding/json"

	extsdk "github.com/fagerbergj/quack-extensions/sdk"

	"github.com/fagerbergj/quack/internal/schema"
)

// chatOrigin decodes a chat row's Origin JSON (an *extsdk.ChatOrigin from an extension's Dispatch) into the
// wire schema. Nil on no origin, a malformed blob, or a missing required field; never a partial chip.
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
		// Labels' element type is an anonymous generated struct; going through wireLabelValue avoids
		// matching its field order and leaves unset Display/Href absent rather than "".
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

// wireLabelValue mirrors the openapi ChatOrigin.labels element's JSON shape,
// bridged onto the generated anonymous struct by a JSON round-trip.
type wireLabelValue struct {
	Value   string `json:"value"`
	Display string `json:"display,omitempty"`
	Href    string `json:"href,omitempty"`
}
