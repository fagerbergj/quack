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
	if e.InputTokens != 0 || e.OutputTokens != 0 {
		resp.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     int32(e.InputTokens),
			CandidatesTokenCount: int32(e.OutputTokens),
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
		ae := invokeAgentEntry{ts: e.At, agentName: e.Agent}
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

// nearMissChat builds the near-miss diff for a chat divergence at pos.
func nearMissChat(chat []chatEntry, pos int) []NearMiss {
	var out []NearMiss
	for _, i := range []int{pos - 1, pos, pos + 1} {
		if i < 0 || i >= len(chat) {
			continue
		}
		out = append(out, NearMiss{Position: i, Name: chat[i].RequestModel, Field: "model"})
	}
	return out
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
	var out []NearMiss
	for _, i := range []int{pos - 1, pos, pos + 1} {
		if i < 0 || i >= len(agents) {
			continue
		}
		out = append(out, NearMiss{Position: i, Name: agents[i].agentName, Field: "agent"})
	}
	return out
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
