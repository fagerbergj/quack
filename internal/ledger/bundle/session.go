package bundle

import (
	"encoding/json"
	"time"

	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/ledger"
)

// StreamKey identifies one sequential stream of ledger events (node, agent, round).
type StreamKey struct {
	Node  string
	Agent string
	Round string
}

func (k StreamKey) String() string { return k.Node + "/" + k.Agent + "/" + k.Round }

// chatEntry is one recorded llm.call entry.
type chatEntry struct {
	ts time.Time
	ledger.LLMCallPayload
}

// invokeAgentEntry is one recorded ACP subprocess round's full protocol conversation.
type invokeAgentEntry struct {
	ts        time.Time
	agentName string
	sent      []json.RawMessage // client → agent (quack's requests)
	received  []json.RawMessage // agent → client (session/updates + responses)
	plugins   []ledger.PluginRef
	artifacts []ledger.ArtifactRef
}

// EvalScore is one recorded judge verdict.
type EvalScore struct {
	Node        string
	Round       string
	ResponseID  string
	Criterion   string
	Score       float64
	Explanation string
	Timestamp   time.Time
}

// streamState is one StreamKey's recorded activity.
type streamState struct {
	chat   []chatEntry
	agents []invokeAgentEntry
}

// Session is a loaded bundle, queryable for the shapes eval/dataset/experiment need.
type Session struct {
	manifest ledger.Manifest
	streams  map[StreamKey]*streamState

	// Earliest recorded chat input (UserTurns derives end-user turns from the root stream).
	earliestChatInput string
	haveEarliest      bool
	earliestTS        time.Time

	// Recorded judge verdicts (collected outside per-stream sequences).
	evalScores []EvalScore
}

func (s *Session) state(key StreamKey) *streamState {
	st, ok := s.streams[key]
	if !ok {
		st = &streamState{}
		s.streams[key] = st
	}
	return st
}

// ingest files one observation entry into its stream by kind. Intent kinds
// and tool calls carry nothing NodeRuns/UserTurns/EvaluationResults need and are dropped.
func (s *Session) ingest(e ledger.Entry) {
	key := StreamKey{Node: e.NodeID, Agent: e.Agent, Round: e.Round}
	switch e.Kind {
	case ledger.KindEvalScore:
		var p ledger.EvalScorePayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		s.evalScores = append(s.evalScores, EvalScore{Node: e.NodeID, Round: e.Round, ResponseID: p.ResponseID,
			Criterion: p.Criterion, Score: p.Score, Explanation: p.Explanation, Timestamp: e.At})
	case ledger.KindLLMCall:
		ce := chatEntry{ts: e.At}
		if json.Unmarshal(e.Payload, &ce.LLMCallPayload) != nil {
			return
		}
		st := s.state(key)
		st.chat = append(st.chat, ce)
		if !s.haveEarliest || ce.ts.Before(s.earliestTS) {
			s.haveEarliest, s.earliestTS, s.earliestChatInput = true, ce.ts, ce.Input
		}
	case ledger.KindAgentInvoke:
		var p ledger.AgentInvokePayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		ae := invokeAgentEntry{ts: e.At, agentName: e.Agent, plugins: p.Plugins, artifacts: p.Artifacts}
		if p.Sent != "" {
			_ = json.Unmarshal([]byte(p.Sent), &ae.sent)
		}
		if p.Received != "" {
			_ = json.Unmarshal([]byte(p.Received), &ae.received)
		}
		st := s.state(key)
		st.agents = append(st.agents, ae)
	}
}

// finalize sorts every stream's sequences into timestamp order.
func (s *Session) finalize() {
	for _, st := range s.streams {
		sortByTime(st.chat, func(e chatEntry) time.Time { return e.ts })
		sortByTime(st.agents, func(e invokeAgentEntry) time.Time { return e.ts })
	}
	sortByTime(s.evalScores, func(e EvalScore) time.Time { return e.Timestamp })
}

// rootStream is the top-level orchestrator/planner conversation (zero-value StreamKey).
func (s *Session) rootStream() (*streamState, bool) {
	st, ok := s.streams[StreamKey{}]
	return st, ok
}

// NodeRun is one node's delivered task/answer - the shape `quack dataset
// export` needs per gated node run. A node with several rounds (draft,
// continuations, revises) collapses to one NodeRun: see NodeRuns.
type NodeRun struct {
	Task   string
	Answer string
	// At is the answer's recorded timestamp - lets callers order/pick among
	// several nodes by recency instead of key name.
	At time.Time
	// Metadata off the answer round's last recorded llm.call (or, for an ACP
	// round, zero) - provenance for a dataset item.
	PromptSource    string
	PromptVersionID string
	QuackVersion    string
	// Artifacts/Plugins: every artifact/plugin the answer round resolved
	// the full list PromptSource/PromptVersionID/QuackVersion summarize.
	Artifacts []ledger.ArtifactRef
	Plugins   []ledger.PluginRef
}

// roundRun is one (Node, Agent, Round) stream's task/answer before rounds
// collapse into their node's single NodeRun.
type roundRun struct {
	key           StreamKey
	task          string
	taskAt        time.Time
	answer        string
	answerAt      time.Time
	promptSource  string
	promptVersion string
	quackVersion  string
	artifacts     []ledger.ArtifactRef
	plugins       []ledger.PluginRef
}

// NodeRuns returns one NodeRun per non-root node whose Agent is in agents:
// task from its earliest round (the draft, never a synthetic revise prompt),
// answer from its latest round by timestamp (what the node delivered).
func (s *Session) NodeRuns(agents map[string]bool) map[StreamKey]NodeRun {
	byNode := map[string][]roundRun{}
	for key, st := range s.streams {
		if key.Node == "" || !agents[key.Agent] {
			continue
		}
		if rr, ok := chatRoundRun(key, st); ok {
			byNode[key.Node] = append(byNode[key.Node], rr)
		} else if rr, ok := acpRoundRun(key, st); ok {
			byNode[key.Node] = append(byNode[key.Node], rr)
		}
	}

	out := map[StreamKey]NodeRun{}
	for _, rounds := range byNode {
		earliest, latest := rounds[0], rounds[0]
		for _, rr := range rounds[1:] {
			if rr.taskAt.Before(earliest.taskAt) {
				earliest = rr
			}
			if rr.answerAt.After(latest.answerAt) {
				latest = rr
			}
		}
		out[latest.key] = NodeRun{
			Task: earliest.task, Answer: latest.answer, At: latest.answerAt,
			PromptSource: latest.promptSource, PromptVersionID: latest.promptVersion, QuackVersion: latest.quackVersion,
			Artifacts: latest.artifacts, Plugins: latest.plugins,
		}
	}
	return out
}

// chatRoundRun builds a round's task/answer from its native llm.call stream.
func chatRoundRun(key StreamKey, st *streamState) (roundRun, bool) {
	if len(st.chat) == 0 {
		return roundRun{}, false
	}
	rr := roundRun{key: key, taskAt: st.chat[0].ts}
	if texts := userTexts(st.chat[0].Input); len(texts) > 0 {
		rr.task = texts[len(texts)-1]
	}
	for i := len(st.chat) - 1; i >= 0; i-- {
		if st.chat[i].Output == "" {
			continue
		}
		var c genai.Content
		if json.Unmarshal([]byte(st.chat[i].Output), &c) == nil {
			if text := partsText(c.Parts); text != "" {
				rr.answer, rr.answerAt = text, st.chat[i].ts
				rr.promptSource, rr.promptVersion, rr.quackVersion = st.chat[i].PromptSource, st.chat[i].PromptVersionID, st.chat[i].QuackVersion
				rr.artifacts, rr.plugins = st.chat[i].Artifacts, st.chat[i].Plugins
				return rr, true
			}
		}
	}
	return roundRun{}, false // chat rows present but none carried a usable answer
}

// acpRoundRun builds a round's task/answer from its invoke_agent stream - an
// ACP round (e.g. code-reviewer) emits no llm.call, only one invoke_agent
// record per round (internal/acp/emit.go), so it needs its own extraction.
func acpRoundRun(key StreamKey, st *streamState) (roundRun, bool) {
	if len(st.agents) == 0 {
		return roundRun{}, false
	}
	ae := st.agents[len(st.agents)-1]
	task, answer := acpTaskAndAnswer(ae)
	if task == "" && answer == "" {
		return roundRun{}, false
	}
	return roundRun{key: key, task: task, taskAt: ae.ts, answer: answer, answerAt: ae.ts, artifacts: ae.artifacts, plugins: ae.plugins}, true
}

// acpFrame is the JSON-RPC envelope shared by every ACP wire message.
type acpFrame struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// acpContentBlock is the ACP ContentBlock fields NodeRuns needs (text only).
type acpContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// acpTaskAndAnswer extracts the sent session/prompt's text (task) and the
// received agent_message_chunk stream's concatenated text (answer) from one
// invoke_agent round's teed protocol frames.
func acpTaskAndAnswer(ae invokeAgentEntry) (task, answer string) {
	for _, raw := range ae.sent {
		var f acpFrame
		if json.Unmarshal(raw, &f) != nil || f.Method != "session/prompt" {
			continue
		}
		var params struct {
			Prompt []acpContentBlock `json:"prompt"`
		}
		if json.Unmarshal(f.Params, &params) != nil {
			continue
		}
		var b []byte
		for _, blk := range params.Prompt {
			if blk.Type == "text" {
				b = append(b, []byte(blk.Text)...)
			}
		}
		task = string(b)
		break
	}
	var b []byte
	for _, raw := range ae.received {
		var f acpFrame
		if json.Unmarshal(raw, &f) != nil || f.Method != "session/update" {
			continue
		}
		var params struct {
			Update struct {
				SessionUpdate string          `json:"sessionUpdate"`
				Content       acpContentBlock `json:"content"`
			} `json:"update"`
		}
		if json.Unmarshal(f.Params, &params) != nil {
			continue
		}
		switch params.Update.SessionUpdate {
		case "tool_call":
			// Delivered answer is text after the last tool call only (mirrors translate.go's answer.Reset()).
			b = nil
		case "agent_message_chunk":
			if params.Update.Content.Type == "text" {
				b = append(b, []byte(params.Update.Content.Text)...)
			}
		}
	}
	answer = string(b)
	return task, answer
}

// UserTurns returns every recorded end-user turn from the root stream, oldest first.
func (s *Session) UserTurns() []string {
	st, ok := s.rootStream()
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	var turns []string
	for _, ce := range st.chat {
		for _, text := range userTexts(ce.Input) {
			if seen[text] {
				continue
			}
			seen[text] = true
			turns = append(turns, text)
		}
	}
	return turns
}

// userTexts returns every role:user message's concatenated text.
func userTexts(inputJSON string) []string {
	if inputJSON == "" {
		return nil
	}
	var contents []*genai.Content
	if err := json.Unmarshal([]byte(inputJSON), &contents); err != nil {
		return nil
	}
	var out []string
	for _, c := range contents {
		if c == nil || c.Role != genai.RoleUser {
			continue
		}
		if text := partsText(c.Parts); text != "" {
			out = append(out, text)
		}
	}
	return out
}

// partsText concatenates a Content's text parts.
func partsText(parts []*genai.Part) string {
	var b []byte
	for _, p := range parts {
		if p != nil && p.Text != "" {
			b = append(b, []byte(p.Text)...)
		}
	}
	return string(b)
}

// FinalAnswer returns the newest recorded model output in the root stream.
// Same extraction applied to both recorded and fresh runs for apples-to-apples comparison.
func (s *Session) FinalAnswer() (string, bool) {
	st, ok := s.rootStream()
	if !ok {
		return "", false
	}
	for i := len(st.chat) - 1; i >= 0; i-- {
		ce := st.chat[i]
		if ce.Output == "" {
			continue
		}
		var c genai.Content
		if err := json.Unmarshal([]byte(ce.Output), &c); err != nil {
			continue
		}
		if text := partsText(c.Parts); text != "" {
			return text, true
		}
	}
	return "", false
}

// EvaluationResults returns every recorded evaluation event, oldest first.
func (s *Session) EvaluationResults() []EvalScore {
	out := make([]EvalScore, len(s.evalScores))
	copy(out, s.evalScores)
	return out
}
