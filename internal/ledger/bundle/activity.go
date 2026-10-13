package bundle

import (
	"encoding/json"

	"google.golang.org/genai"
)

// Activity is one node's worker effort across all its rounds: what a cheaper prompt spends less of.
type Activity struct {
	ModelTurns int
	ToolCalls  int
	// RejectedToolCalls returned an error to the model: a schema-invalid or unknown call, or one that failed.
	RejectedToolCalls int
}

// NodeActivity counts agent's model turns and tool calls on node over every round; the judge's
// streams are agent "judge", so they never count. Native rounds read llm.call rows, ACP rounds their frames.
func (s *Session) NodeActivity(node, agent string) Activity {
	var a Activity
	for key, st := range s.streams {
		if key.Node != node || key.Agent != agent {
			continue
		}
		a.add(chatActivity(st.chat))
		for _, ae := range st.agents {
			a.add(acpActivity(ae.received))
		}
	}
	return a
}

func (a *Activity) add(b Activity) {
	a.ModelTurns += b.ModelTurns
	a.ToolCalls += b.ToolCalls
	a.RejectedToolCalls += b.RejectedToolCalls
}

// chatActivity: one llm.call is one model turn. Inputs replay the whole history, so an
// errored function response counts once by its call id, not once per later turn.
func chatActivity(calls []chatEntry) Activity {
	a := Activity{ModelTurns: len(calls)}
	rejected := map[string]bool{}
	for _, ce := range calls {
		var out genai.Content
		if json.Unmarshal([]byte(ce.Output), &out) == nil {
			for _, p := range out.Parts {
				if p != nil && p.FunctionCall != nil {
					a.ToolCalls++
				}
			}
		}
		var in []*genai.Content
		if json.Unmarshal([]byte(ce.Input), &in) != nil {
			continue
		}
		for _, c := range in {
			for _, fr := range errorResponses(c) {
				rejected[fr.ID+"/"+fr.Name] = true
			}
		}
	}
	a.RejectedToolCalls = len(rejected)
	return a
}

// errorResponses: c's function responses that carried an error back to the model.
func errorResponses(c *genai.Content) []*genai.FunctionResponse {
	if c == nil {
		return nil
	}
	var out []*genai.FunctionResponse
	for _, p := range c.Parts {
		if p != nil && p.FunctionResponse != nil && p.FunctionResponse.Response["error"] != nil {
			out = append(out, p.FunctionResponse)
		}
	}
	return out
}

// acpActivity reads one ACP round's received frames. ACP has no per-turn frame, so a turn is a run of
// model output (thought, message or usage updates) between tool activity; calls dedup by id across updates.
func acpActivity(received []json.RawMessage) Activity {
	var a Activity
	calls, failed := map[string]bool{}, map[string]bool{}
	inModel := false
	for _, raw := range received {
		var f acpFrame
		if json.Unmarshal(raw, &f) != nil || f.Method != "session/update" {
			continue
		}
		var params struct {
			Update struct {
				SessionUpdate string `json:"sessionUpdate"`
				Status        string `json:"status"`
				ToolCallID    string `json:"toolCallId"`
			} `json:"update"`
		}
		if json.Unmarshal(f.Params, &params) != nil {
			continue
		}
		switch u := params.Update; u.SessionUpdate {
		case "agent_thought_chunk", "agent_message_chunk", "usage_update":
			if !inModel {
				a.ModelTurns++
			}
			inModel = true
		case "tool_call":
			calls[u.ToolCallID] = true
			inModel = false
		case "tool_call_update":
			calls[u.ToolCallID] = true // ACP lets an update create a call no tool_call frame announced
			if u.Status == "failed" {
				failed[u.ToolCallID] = true
			}
			inModel = false
		}
	}
	a.ToolCalls, a.RejectedToolCalls = len(calls), len(failed)
	return a
}
