package replay

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"google.golang.org/adk/v2/model"
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

func streamKeyFor(c ledger.Coords) StreamKey {
	return StreamKey{Node: c.Node, Agent: c.Agent, Round: c.Round}
}

// chatEntry is one recorded llm.call entry.
type chatEntry struct {
	ts time.Time
	ledger.LLMCallPayload
}

// toResponse reconstructs the *model.LLMResponse the live call would have produced.
func (e chatEntry) toResponse() *model.LLMResponse {
	resp := &model.LLMResponse{
		ModelVersion: e.ResponseModel,
		FinishReason: genai.FinishReason(e.FinishReason),
		TurnComplete: true,
	}
	if e.Output != "" {
		var c genai.Content
		if err := json.Unmarshal([]byte(e.Output), &c); err == nil {
			resp.Content = &c
		}
	}
	if e.InputTokens != 0 || e.OutputTokens != 0 || e.CachedTokens != 0 {
		// InputTokens already excludes CachedTokens (inference.splitPromptTokens) -
		// add it back so PromptTokenCount reconstructs the real raw total a live
		// re-run's genai response would report.
		resp.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:        int32(e.InputTokens + e.CachedTokens),
			CandidatesTokenCount:    int32(e.OutputTokens),
			CachedContentTokenCount: int32(e.CachedTokens),
		}
	}
	return resp
}

// toolEntry is one recorded "execute_tool" operation.
type toolEntry struct {
	ts     time.Time
	result map[string]any
	errStr string
}

// invokeAgentEntry is one recorded ACP subprocess round's full protocol conversation.
type invokeAgentEntry struct {
	ts        time.Time
	agentName string
	sent      []json.RawMessage // client → agent (quack's requests)
	received  []json.RawMessage // agent → client (session/updates + responses)
	plugins   []ledger.PluginRef
}

// EvalScore is one recorded judge verdict. Not part of any replay stream (no operation.name).
type EvalScore struct {
	Node        string
	Round       string
	ResponseID  string
	Criterion   string
	Score       float64
	Explanation string
	Timestamp   time.Time
}

// streamState is one StreamKey's recorded activity plus live consumption cursors.
// forked is fork mode's per-stream sticky bit (see Session.forkCheck).
type streamState struct {
	chat     []chatEntry
	chatPos  int
	tools    map[string][]toolEntry
	toolPos  map[string]int
	agents   []invokeAgentEntry
	agentPos int
	forked   bool
}

// Mode selects a Session's replay semantics.
type Mode string

const (
	ModeStrict Mode = "strict" // never makes a live call; miss = failure
	ModeFork   Mode = "fork"   // serves recorded prefix, goes live on divergence
)

// Session is a loaded, replayable bundle with concurrency-safe consumption cursors.
type Session struct {
	mu       sync.Mutex
	manifest ledger.Manifest
	streams  map[StreamKey]*streamState

	// Earliest recorded chat input (UserTurn derives the user message from it).
	earliestChatInput string
	haveEarliest      bool
	earliestTS        time.Time

	// Recorded judge verdicts (collected outside replay streams).
	evalScores []EvalScore

	drift    []PromptDrift
	failures []*MissError
	forks    []*ForkSignal

	// Fork-replay triggers: mode switches semantics; forkFrom forces live on that node.
	mode     Mode
	forkFrom string
}

// EnableFork switches s to fork-replay mode. forkFromNode forces live at that boundary;
// "" forks on first structural miss. Call once before driving any Next*.
func (s *Session) EnableFork(forkFromNode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = ModeFork
	s.forkFrom = forkFromNode
}

// Mode reports s's current replay mode.
func (s *Session) Mode() Mode {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mode == "" {
		return ModeStrict
	}
	return s.mode
}

// forkCheck returns a ForkSignal when key's stream should go live (already forked or fork-from).
func (s *Session) forkCheck(key StreamKey, st *streamState) *ForkSignal {
	if s.mode != ModeFork {
		return nil
	}
	if st.forked {
		return &ForkSignal{Stream: key, Reason: "sticky"}
	}
	if s.forkFrom != "" && key.Node == s.forkFrom {
		st.forked = true
		fs := &ForkSignal{Stream: key, Reason: "fork-from"}
		s.forks = append(s.forks, fs)
		return fs
	}
	return nil
}

// forkOrFail: structural miss → fork mode hands off to live; strict mode records failure.
func (s *Session) forkOrFail(key StreamKey, st *streamState, me *MissError) error {
	if s.mode == ModeFork {
		st.forked = true
		fs := &ForkSignal{Stream: key, Reason: "miss", Cause: me}
		s.forks = append(s.forks, fs)
		return fs
	}
	return s.recordFailure(me)
}

func (s *Session) state(key StreamKey) *streamState {
	st, ok := s.streams[key]
	if !ok {
		st = &streamState{tools: map[string][]toolEntry{}, toolPos: map[string]int{}}
		s.streams[key] = st
	}
	return st
}

// ingest files one observation entry into its stream by kind. Evaluation
// scores go to evalScores; intent kinds are not replayed and are dropped.
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
	case ledger.KindToolCall:
		var p ledger.ToolCallPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		te := toolEntry{ts: e.At, errStr: p.Error}
		if p.Result != "" {
			_ = json.Unmarshal([]byte(p.Result), &te.result)
		}
		st := s.state(key)
		st.tools[p.Name] = append(st.tools[p.Name], te)
	case ledger.KindAgentInvoke:
		var p ledger.AgentInvokePayload
		if json.Unmarshal(e.Payload, &p) != nil {
			return
		}
		ae := invokeAgentEntry{ts: e.At, agentName: e.Agent, plugins: p.Plugins}
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
		for name, entries := range st.tools {
			sortByTime(entries, func(e toolEntry) time.Time { return e.ts })
			st.tools[name] = entries
		}
		sortByTime(st.agents, func(e invokeAgentEntry) time.Time { return e.ts })
	}
	sortByTime(s.evalScores, func(e EvalScore) time.Time { return e.Timestamp })
}

// UserTurn returns the newest role:user message from the earliest recorded chat call.
func (s *Session) UserTurn() (string, bool) {
	if !s.haveEarliest || s.earliestChatInput == "" {
		return "", false
	}
	var contents []*genai.Content
	if err := json.Unmarshal([]byte(s.earliestChatInput), &contents); err != nil {
		return "", false
	}
	for i := len(contents) - 1; i >= 0; i-- {
		c := contents[i]
		if c == nil || c.Role != genai.RoleUser {
			continue
		}
		var b []byte
		for _, p := range c.Parts {
			if p != nil && p.Text != "" {
				b = append(b, []byte(p.Text)...)
			}
		}
		if len(b) > 0 {
			return string(b), true
		}
	}
	return "", false
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
	return roundRun{key: key, task: task, taskAt: ae.ts, answer: answer, answerAt: ae.ts}, true
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
		if params.Update.SessionUpdate == "agent_message_chunk" && params.Update.Content.Type == "text" {
			b = append(b, []byte(params.Update.Content.Text)...)
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

// Plugins returns every plugin's provenance recorded on any agent.invoke
// entry, name -> sha (#1427 P4: one set per bundle, not per round). A name
// recorded at two different shas refuses rather than guessing.
func (s *Session) Plugins() (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for _, st := range s.streams {
		for _, ae := range st.agents {
			for _, pr := range ae.plugins {
				prev, ok := out[pr.Name]
				if !ok {
					out[pr.Name] = pr.SHA
					continue
				}
				if prev != pr.SHA {
					return nil, fmt.Errorf("replay: plugin %q moved shas mid-run (%s vs %s); refusing to guess which round to pin", pr.Name, prev, pr.SHA)
				}
			}
		}
	}
	return out, nil
}

// contentHash mirrors inference/emit.go's prompt-version hash (duplicated, not imported;
// algorithm must stay identical for drift comparison to mean anything).
func contentHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// recordFailure appends err to accumulated failures and returns it.
func (s *Session) recordFailure(err *MissError) *MissError {
	s.failures = append(s.failures, err)
	return err
}

// nearMissAround is the pos-1..pos+1 diff shared by nearMissChat/nearMissAgent.
func nearMissAround(pos, n int, field string, nameAt func(int) string) []NearMiss {
	var out []NearMiss
	for _, i := range []int{pos - 1, pos, pos + 1} {
		if i < 0 || i >= n {
			continue
		}
		out = append(out, NearMiss{Position: i, Name: nameAt(i), Field: field})
	}
	return out
}

// nearMissChat builds the near-miss diff for a chat divergence at pos.
func nearMissChat(chat []chatEntry, pos int) []NearMiss {
	return nearMissAround(pos, len(chat), "model", func(i int) string { return chat[i].RequestModel })
}

// NextChat consumes the next recorded chat entry, enforcing sequence + modelName match.
// sysInstrJSON hash is compared as informational drift, not a failure.
func (s *Session) NextChat(coords ledger.Coords, modelName string, sysInstrJSON []byte) (*model.LLMResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := streamKeyFor(coords)
	st := s.state(key)
	if fs := s.forkCheck(key, st); fs != nil {
		return nil, fs
	}
	pos := st.chatPos
	if pos >= len(st.chat) {
		return nil, s.forkOrFail(key, st, &MissError{Class: ClassExtra, Stream: key, Op: "chat", Position: pos, Want: modelName, Diff: nearMissChat(st.chat, pos)})
	}
	ce := st.chat[pos]
	if ce.RequestModel != modelName {
		return nil, s.forkOrFail(key, st, &MissError{Class: ClassMismatched, Stream: key, Op: "chat", Position: pos, Want: modelName, Diff: nearMissChat(st.chat, pos)})
	}
	st.chatPos++

	if len(sysInstrJSON) > 0 && ce.PromptVersion != "" {
		if live := contentHash(sysInstrJSON); live != ce.PromptVersion {
			s.drift = append(s.drift, PromptDrift{Stream: key, Position: pos, Recorded: ce.PromptVersion, Live: live})
		}
	}
	return ce.toResponse(), nil
}

// NextToolResult consumes the next recorded execute_tool entry. args accepted for interface
// completeness but not compared (shallow identity: name only).
func (s *Session) NextToolResult(coords ledger.Coords, toolName string, _ any) (map[string]any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := streamKeyFor(coords)
	st := s.state(key)
	if fs := s.forkCheck(key, st); fs != nil {
		return nil, fs
	}
	entries := st.tools[toolName]
	pos := st.toolPos[toolName]
	if pos >= len(entries) {
		return nil, s.forkOrFail(key, st, &MissError{Class: ClassExtra, Stream: key, Op: toolName, Position: pos, Want: toolName, Diff: nearMissTool(st, toolName)})
	}
	st.toolPos[toolName] = pos + 1
	te := entries[pos]
	if te.errStr != "" {
		return nil, fmt.Errorf("replay: recorded tool error: %s", te.errStr)
	}
	return te.result, nil
}

// NextInvokeAgent consumes the next recorded invoke_agent entry. Returns raw ndjson frames.
func (s *Session) NextInvokeAgent(coords ledger.Coords, agentName string) (sent, received []json.RawMessage, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	key := streamKeyFor(coords)
	st := s.state(key)
	if fs := s.forkCheck(key, st); fs != nil {
		return nil, nil, fs
	}
	pos := st.agentPos
	if pos >= len(st.agents) {
		return nil, nil, s.forkOrFail(key, st, &MissError{Class: ClassExtra, Stream: key, Op: "invoke_agent", Position: pos, Want: agentName, Diff: nearMissAgent(st.agents, pos)})
	}
	ae := st.agents[pos]
	if ae.agentName != agentName {
		return nil, nil, s.forkOrFail(key, st, &MissError{Class: ClassMismatched, Stream: key, Op: "invoke_agent", Position: pos, Want: agentName, Diff: nearMissAgent(st.agents, pos)})
	}
	st.agentPos++
	return ae.sent, ae.received, nil
}

// nearMissAgent builds the near-miss diff for an invoke_agent divergence.
func nearMissAgent(agents []invokeAgentEntry, pos int) []NearMiss {
	return nearMissAround(pos, len(agents), "agent", func(i int) string { return agents[i].agentName })
}

// nearMissTool builds the near-miss diff: other unconsumed tools in this stream.
func nearMissTool(st *streamState, toolName string) []NearMiss {
	var out []NearMiss
	for name, entries := range st.tools {
		if name == toolName {
			continue
		}
		if p := st.toolPos[name]; p < len(entries) {
			out = append(out, NearMiss{Position: p, Name: name, Field: "tool"})
		}
	}
	return out
}

// Report returns the session's divergence accounting. Report().Clean() for clean replay.
func (s *Session) Report() Report {
	s.mu.Lock()
	defer s.mu.Unlock()

	var r Report
	for key, st := range s.streams {
		if len(st.chat) > 0 || st.chatPos > 0 {
			r.Streams = append(r.Streams, StreamReport{Stream: key, Op: "chat", Consumed: st.chatPos, Total: len(st.chat)})
		}
		for name, entries := range st.tools {
			r.Streams = append(r.Streams, StreamReport{Stream: key, Op: name, Consumed: st.toolPos[name], Total: len(entries)})
		}
		if len(st.agents) > 0 || st.agentPos > 0 {
			r.Streams = append(r.Streams, StreamReport{Stream: key, Op: "invoke_agent", Consumed: st.agentPos, Total: len(st.agents)})
		}
	}
	r.Drift = append(r.Drift, s.drift...)
	r.Failures = append(r.Failures, s.failures...)
	r.Forked = append(r.Forked, s.forks...)
	return r
}
