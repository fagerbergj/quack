package bundle

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/ledger"
)

// StreamKey identifies one sequential stream of ledger events (node, agent, round).
type StreamKey struct {
	Node  string
	Agent string
	Round string
	// Turn is set only on NodeRuns keys: a recurring job reruns one node id under the same round ids every turn.
	Turn string
}

func (k StreamKey) String() string { return k.Node + "/" + k.Agent + "/" + k.Round + "/" + k.Turn }

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

// ToolCall is one recorded tool.call row.
type ToolCall struct {
	Node string
	At   time.Time
	ledger.ToolCallPayload
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
	toolCalls  []ToolCall

	// turnStarts: each node's fresh node.started entries, oldest first - the turn boundaries NodeRuns splits on.
	turnStarts map[string][]turnStart
}

type turnStart struct {
	at   time.Time
	turn string
}

func (s *Session) state(key StreamKey) *streamState {
	st, ok := s.streams[key]
	if !ok {
		st = &streamState{}
		s.streams[key] = st
	}
	return st
}

// ingest files one observation entry into its stream by kind; intent kinds are dropped.
// Tool calls are kept aside, for judge replay's review-stage rebuild.
func (s *Session) ingest(e ledger.Entry) {
	key := StreamKey{Node: e.NodeID, Agent: e.Agent, Round: e.Round}
	switch e.Kind {
	case ledger.KindNodeStarted:
		s.addTurnStart(e)
	case ledger.KindEvalScore:
		var p ledger.EvalScorePayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		s.evalScores = append(s.evalScores, EvalScore{Node: e.NodeID, Round: e.Round, ResponseID: p.ResponseID,
			Criterion: p.Criterion, Score: p.Score, Explanation: p.Explanation, Timestamp: e.At})
	case ledger.KindToolCall:
		tc := ToolCall{Node: e.NodeID, At: e.At}
		if json.Unmarshal(e.Payload, &tc.ToolCallPayload) == nil {
			s.toolCalls = append(s.toolCalls, tc)
		}
	case ledger.KindLLMCall:
		ce := chatEntry{ts: e.At}
		// An ACP call's row carries usage only; its round's content is read from the agent.invoke stream.
		if json.Unmarshal(e.Payload, &ce.LLMCallPayload) != nil || ce.Input == "" && ce.Output == "" {
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
	sortByTime(s.toolCalls, func(e ToolCall) time.Time { return e.At })
	for _, starts := range s.turnStarts {
		sortByTime(starts, func(e turnStart) time.Time { return e.at })
	}
}

// addTurnStart records a fresh node.started as a turn boundary. A resumed or continued start opens no
// turn, so its rounds fold into the run they carry on: the original task and the final answer.
func (s *Session) addTurnStart(e ledger.Entry) {
	var p struct {
		ResumedFrom string `json:"resumed_from"`
		Continued   bool   `json:"continued"`
	}
	if json.Unmarshal(e.Payload, &p) == nil && (p.ResumedFrom != "" || p.Continued) {
		return
	}
	if s.turnStarts == nil {
		s.turnStarts = map[string][]turnStart{}
	}
	s.turnStarts[e.NodeID] = append(s.turnStarts[e.NodeID], turnStart{at: e.At, turn: e.TurnID})
}

// turnAt is the turn of node's latest fresh start at or before ts; "" before any, or with none recorded.
func (s *Session) turnAt(node string, ts time.Time) string {
	starts := s.turnStarts[node]
	i := sort.Search(len(starts), func(i int) bool { return starts[i].at.After(ts) })
	if i == 0 {
		return ""
	}
	return starts[i-1].turn
}

// byTurn splits node's stream st by the turn each entry was recorded in, keeping their time order.
func (s *Session) byTurn(node string, st *streamState) map[string]*streamState {
	out := map[string]*streamState{}
	part := func(ts time.Time) *streamState {
		turn := s.turnAt(node, ts)
		if out[turn] == nil {
			out[turn] = &streamState{}
		}
		return out[turn]
	}
	for _, c := range st.chat {
		p := part(c.ts)
		p.chat = append(p.chat, c)
	}
	for _, a := range st.agents {
		p := part(a.ts)
		p.agents = append(p.agents, a)
	}
	return out
}

// rootStream is the top-level orchestrator/planner conversation (zero-value StreamKey).
func (s *Session) rootStream() (*streamState, bool) {
	st, ok := s.streams[StreamKey{}]
	return st, ok
}

// NodeRun is one node's delivered task/answer, the shape `quack dataset export` needs; a node's several
// rounds collapse into one (see NodeRuns).
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

// NodeRuns returns one NodeRun per non-root node and turn whose Agent is in agents: task from its earliest
// round (never a synthetic revise prompt), answer from its latest.
func (s *Session) NodeRuns(agents map[string]bool) map[StreamKey]NodeRun {
	byNode := map[[2]string][]roundRun{}
	for key, whole := range s.streams {
		if key.Node == "" || !agents[key.Agent] {
			continue
		}
		for turn, st := range s.byTurn(key.Node, whole) {
			k := key
			k.Turn = turn
			rr, ok := chatRoundRun(k, st)
			if !ok {
				rr, ok = acpRoundRun(k, st)
			}
			if ok {
				nt := [2]string{k.Node, turn}
				byNode[nt] = append(byNode[nt], rr)
			}
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
		rr.task = gateTask(texts[len(texts)-1])
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

// gatePromptSaid is how ADK renders the gate's task message (author stream.GatePromptAuthor) to the worker.
const gatePromptSaid = "[quack-gate] said: "

// gateTask strips ADK's "run" / "For context:" framing off a native round's first input, leaving the
// task the gate built: the bytes a replay of the node (experiment run) must send again.
func gateTask(text string) string {
	if _, task, ok := strings.Cut(text, gatePromptSaid); ok {
		return task
	}
	return text
}

// acpRoundRun builds a round's task/answer from its invoke_agent stream: an ACP round's llm.call rows carry usage only.
func acpRoundRun(key StreamKey, st *streamState) (roundRun, bool) {
	if len(st.agents) == 0 {
		return roundRun{}, false
	}
	task, _ := acpTaskAndAnswer(st.agents[0])
	// Provenance defaults to the last invocation: a round that ended on a tool call answers nothing but still
	// resolved its prompt, and an experiment's prompt pin reads it from here.
	last := st.agents[len(st.agents)-1]
	rr := roundRun{key: key, task: task, taskAt: st.agents[0].ts, artifacts: last.artifacts, plugins: last.plugins}
	// The latest invocation that answered: a later answerless one (an empty continuation, a post-judge stage
	// round) must not blank it, and its zero answerAt never outranks another stream's answer in NodeRuns.
	for i := len(st.agents) - 1; i >= 0; i-- {
		if _, answer := acpTaskAndAnswer(st.agents[i]); answer != "" {
			ae := st.agents[i]
			rr.answer, rr.answerAt, rr.artifacts, rr.plugins = answer, ae.ts, ae.artifacts, ae.plugins
			break
		}
	}
	return rr, rr.task != "" || rr.answer != ""
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

// acpTaskAndAnswer extracts the session/prompt text (task) and the concatenated agent_message_chunk text
// (answer) from one round's teed protocol frames.
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

// partsText is the reply as the gate saw it: reasoning parts (thought: true) never reached the judge or user.
func partsText(parts []*genai.Part) string {
	var b []byte
	for _, p := range parts {
		if p != nil && p.Text != "" && !p.Thought {
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

// LastModel is the request model of agent's latest llm.call on node, "" when none was kept: an ACP
// round's usage-only rows are dropped, so its model comes off the node's dag_nodes row instead.
func (s *Session) LastModel(node, agent string) string {
	var at time.Time
	model := ""
	for key, st := range s.streams {
		if key.Node != node || key.Agent != agent {
			continue
		}
		for _, ce := range st.chat {
			if ce.RequestModel != "" && !ce.ts.Before(at) {
				at, model = ce.ts, ce.RequestModel
			}
		}
	}
	return model
}

// EvaluationResults returns every recorded evaluation event, oldest first.
func (s *Session) EvaluationResults() []EvalScore {
	out := make([]EvalScore, len(s.evalScores))
	copy(out, s.evalScores)
	return out
}

// ToolCalls returns node's recorded tool calls, oldest first.
func (s *Session) ToolCalls(node string) []ToolCall {
	var out []ToolCall
	for _, tc := range s.toolCalls {
		if tc.Node == node {
			out = append(out, tc)
		}
	}
	return out
}

// Streams returns every recorded (node, agent, round) stream key - the judge
// replay's index into which rounds exist, beyond what NodeRuns collapses to one per node.
func (s *Session) Streams() []StreamKey {
	out := make([]StreamKey, 0, len(s.streams))
	for k := range s.streams {
		out = append(out, k)
	}
	return out
}

// ChatTurn is one recorded llm.call's Input/Output, verbatim JSON strings.
type ChatTurn struct {
	Input, Output string
	At            time.Time
}

// ChatTurns returns key's recorded llm.call Input/Output pairs, oldest first: a round's full tool-loop
// conversation, for judge replay.
func (s *Session) ChatTurns(key StreamKey) []ChatTurn {
	st, ok := s.streams[key]
	if !ok {
		return nil
	}
	out := make([]ChatTurn, len(st.chat))
	for i, ce := range st.chat {
		out[i] = ChatTurn{Input: ce.Input, Output: ce.Output, At: ce.ts}
	}
	return out
}

// RoundTaskAnswer extracts key's task/answer like NodeRuns does per round, without collapsing a node's rounds:
// judge replay needs each round's own answer.
func (s *Session) RoundTaskAnswer(key StreamKey) (task, answer string, at time.Time, ok bool) {
	st, exists := s.streams[key]
	if !exists {
		return "", "", time.Time{}, false
	}
	if rr, ok := chatRoundRun(key, st); ok {
		return rr.task, rr.answer, rr.answerAt, true
	}
	if rr, ok := acpRoundRun(key, st); ok {
		return rr.task, rr.answer, rr.answerAt, true
	}
	return "", "", time.Time{}, false
}
