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

// StreamKey identifies one sequential stream of ledger events: the same
// (node, agent, round) grouping the recorder stamps via ledger.Coords and
// dagStream already groups run ids by - replay's grouping needs nothing new
// (.quack/replay-log.md "Everything replay needs is derived at read time").
type StreamKey struct {
	Node  string
	Agent string
	Round string
}

func (k StreamKey) String() string { return k.Node + "/" + k.Agent + "/" + k.Round }

func streamKeyFor(c ledger.Coords) StreamKey {
	return StreamKey{Node: c.Node, Agent: c.Agent, Round: c.Round}
}

// chatEntry is one recorded "chat" operation within a stream.
type chatEntry struct {
	ts            time.Time
	requestModel  string
	responseModel string
	finishReason  string
	inputTokens   int64
	outputTokens  int64
	promptVersion string
	inputJSON     string // gen_ai.input.messages: JSON array of *genai.Content
	outputJSON    string // gen_ai.output.messages: JSON of one *genai.Content
}

// toResponse reconstructs the *model.LLMResponse a live GenerateContent call
// would have produced - the recorded FINAL assembled response (the same
// thing tracedModel emitted the event from), yielded once with
// TurnComplete: true.
func (e chatEntry) toResponse() *model.LLMResponse {
	resp := &model.LLMResponse{
		ModelVersion: e.responseModel,
		FinishReason: genai.FinishReason(e.finishReason),
		TurnComplete: true,
	}
	if e.outputJSON != "" {
		var c genai.Content
		if err := json.Unmarshal([]byte(e.outputJSON), &c); err == nil {
			resp.Content = &c
		}
	}
	if e.inputTokens != 0 || e.outputTokens != 0 {
		resp.UsageMetadata = &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     int32(e.inputTokens),
			CandidatesTokenCount: int32(e.outputTokens),
		}
	}
	return resp
}

// toolEntry is one recorded "execute_tool" operation for a specific tool
// name within a stream.
type toolEntry struct {
	ts     time.Time
	result map[string]any
	errStr string
}

// invokeAgentEntry is one recorded "invoke_agent" operation - one ACP
// subprocess round's full protocol conversation (acp/emit.go's
// emitInvokeAgent), both directions preserved as raw ndjson frames so
// playback can replay them byte-for-byte without reinterpreting the wire
// format (#604).
type invokeAgentEntry struct {
	ts        time.Time
	agentName string
	sent      []json.RawMessage // client → agent (quack's requests)
	received  []json.RawMessage // agent → client (session/updates + responses)
}

// streamState is one StreamKey's recorded activity plus live consumption
// cursors - chat is one ordered sequence; tools are further keyed by name
// (.quack/replay-log.md: "execute_tool events per stream keyed further by
// gen_ai.tool.name sequence"), each its own ordered sequence. agents (ACP
// rounds) is one more ordered sequence, like chat - a node's ACP worker
// makes at most one invoke_agent call per round, so no further keying needed.
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
	// ModeStrict never makes a live call - a miss is a failure, full stop
	// (.quack/replay-log.md "Forbidden"). The zero value, so a Session no
	// caller ever calls EnableFork on behaves exactly as before #605.
	ModeStrict Mode = "strict"
	// ModeFork serves the recorded prefix, then goes live from the first
	// divergent step (or an explicit --fork-from node boundary) instead of
	// failing - see Session.EnableFork.
	ModeFork Mode = "fork"
)

// Session is a loaded, replayable bundle: stream indexes plus concurrency-
// safe consumption cursors and the accumulating divergence report. A plan's
// nodes run concurrently, but each is its own stream, so the mutex only ever
// serializes UNRELATED streams' bookkeeping, never blocks on real work.
type Session struct {
	mu       sync.Mutex
	manifest Manifest
	streams  map[StreamKey]*streamState

	// earliest, by timestamp, across every recorded chat entry - the input
	// UserTurn derives the recorded user message from
	// (.quack/replay-log.md: "newest role:user message in the root stream's
	// gen_ai.input.messages").
	earliestChatInput string
	haveEarliest      bool
	earliestTS        time.Time

	drift    []PromptDrift
	failures []*MissError
	forks    []*ForkSignal

	// mode/forkFrom are fork-replay's two triggers (EnableFork): mode ==
	// ModeFork switches semantics at all; forkFrom, when non-empty, forces
	// EVERY stream on that node id live from its very first call, whether or
	// not the recording would otherwise have matched it (the CLI's --fork-
	// from - verifying a prompt/plan fix needs a REAL model call, not the
	// old recorded one, even when the call sequence itself never diverges).
	mode     Mode
	forkFrom string
}

// EnableFork switches s into fork-replay mode. forkFromNode, when non-empty,
// forces every stream on that node id live from its first call onward
// (explicit boundary); "" forks purely on the first structural miss any
// stream hits (useful when a deterministic-code fix's downstream ripple
// isn't known in advance). Call once, before driving any Next* call -
// concurrent with them is safe (same mutex) but the FIRST call on a stream
// decides whether that stream is forced live, so enabling fork mid-run only
// affects streams not yet touched.
func (s *Session) EnableFork(forkFromNode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = ModeFork
	s.forkFrom = forkFromNode
}

// Mode reports s's current replay mode (ModeStrict unless EnableFork was called).
func (s *Session) Mode() Mode {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mode == "" {
		return ModeStrict
	}
	return s.mode
}

// forkCheck runs BEFORE consulting the recording at all - caller holds
// s.mu. Returns a *ForkSignal when key's stream should go live without even
// attempting a match: it already forked (sticky), or key.Node is s's
// explicit --fork-from boundary. Returns nil to mean "consult the recording
// normally".
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

// forkOrFail is the OTHER fork trigger: a structural miss just found in
// key's stream. In fork mode this hands off to live (fork-replay's whole
// point) instead of failing the run; in strict mode it's recorded as a
// failure exactly as before #605. Caller holds s.mu.
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

// ingest files one parsed ledger line into its stream, by gen_ai.operation.name.
// Anything else (plan/evaluation.result events, or a record with no
// operation at all) carries nothing replay matches on and is dropped.
func (s *Session) ingest(l line) {
	key := StreamKey{Node: attrStr(l.Attrs, "quack.node"), Agent: attrStr(l.Attrs, "gen_ai.agent.name"), Round: attrStr(l.Attrs, "quack.round")}
	st := s.state(key)
	switch attrStr(l.Attrs, "gen_ai.operation.name") {
	case "chat":
		ce := chatEntry{
			ts:            l.Timestamp,
			requestModel:  attrStr(l.Attrs, "gen_ai.request.model"),
			responseModel: attrStr(l.Attrs, "gen_ai.response.model"),
			finishReason:  attrFirstOf(l.Attrs, "gen_ai.response.finish_reasons"),
			inputTokens:   attrInt64(l.Attrs, "gen_ai.usage.input_tokens"),
			outputTokens:  attrInt64(l.Attrs, "gen_ai.usage.output_tokens"),
			promptVersion: attrStr(l.Attrs, "gen_ai.prompt.version"),
			inputJSON:     attrStr(l.Attrs, "gen_ai.input.messages"),
			outputJSON:    attrStr(l.Attrs, "gen_ai.output.messages"),
		}
		st.chat = append(st.chat, ce)
		if !s.haveEarliest || ce.ts.Before(s.earliestTS) {
			s.haveEarliest, s.earliestTS, s.earliestChatInput = true, ce.ts, ce.inputJSON
		}
	case "execute_tool":
		name := attrStr(l.Attrs, "gen_ai.tool.name")
		te := toolEntry{ts: l.Timestamp, errStr: attrStr(l.Attrs, "error.type")}
		if raw := attrStr(l.Attrs, "gen_ai.tool.call.result"); raw != "" {
			_ = json.Unmarshal([]byte(raw), &te.result)
		}
		st.tools[name] = append(st.tools[name], te)
	case "invoke_agent":
		ae := invokeAgentEntry{ts: l.Timestamp, agentName: attrStr(l.Attrs, "gen_ai.agent.name")}
		if raw := attrStr(l.Attrs, "gen_ai.input.messages"); raw != "" {
			_ = json.Unmarshal([]byte(raw), &ae.sent)
		}
		if raw := attrStr(l.Attrs, "gen_ai.output.messages"); raw != "" {
			_ = json.Unmarshal([]byte(raw), &ae.received)
		}
		st.agents = append(st.agents, ae)
	}
}

// finalize sorts every stream's sequences into timestamp order - a single
// FSStore writer already appends in order, but sorting is what makes the
// contract explicit for any other bundle producer (.quack/replay-log.md:
// "the shipped filesystem ledger is one producer of bundles").
func (s *Session) finalize() {
	for _, st := range s.streams {
		sortByTime(st.chat, func(e chatEntry) time.Time { return e.ts })
		for name, entries := range st.tools {
			sortByTime(entries, func(e toolEntry) time.Time { return e.ts })
			st.tools[name] = entries
		}
		sortByTime(st.agents, func(e invokeAgentEntry) time.Time { return e.ts })
	}
}

// UserTurn returns the newest role:user message text from the earliest
// recorded chat call's input messages - the recorded user turn replaytest
// feeds a live run (.quack/replay-log.md).
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

// contentHash is replay's own copy of inference/emit.go's prompt-version
// hash (sha256 of the system-instruction bytes, truncated) - duplicated,
// not imported, because the two packages compute it over content neither
// owns: inference hashes what it's about to send: replay hashes what it
// reads back. The algorithm must stay identical for drift comparison to
// mean anything.
func contentHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// recordFailure appends err to the session's accumulated failures (caller
// holds s.mu) and returns it, so call sites can `return nil, s.recordFailure(...)`.
func (s *Session) recordFailure(err *MissError) *MissError {
	s.failures = append(s.failures, err)
	return err
}

// nearMissChat builds the near-miss diff for a chat divergence at pos: up to
// one entry on each side of pos, so a MissError never reads as a bare "not
// found" (.quack/replay-log.md).
func nearMissChat(chat []chatEntry, pos int) []NearMiss {
	var out []NearMiss
	for _, i := range []int{pos - 1, pos, pos + 1} {
		if i < 0 || i >= len(chat) {
			continue
		}
		out = append(out, NearMiss{Position: i, Name: chat[i].requestModel, Field: "model"})
	}
	return out
}

// NextChat consumes the next recorded chat entry in coords' stream,
// enforcing sequence + shallow identity (modelName) match. sysInstrJSON is
// the live request's marshaled system instruction (nil/empty if none) - its
// hash is compared to the recorded gen_ai.prompt.version as INFORMATIONAL
// drift, never a failure. A structural miss (extra or mismatched) returns a
// *MissError and is also recorded into the session's Report.
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
	if ce.requestModel != modelName {
		return nil, s.forkOrFail(key, st, &MissError{Class: ClassMismatched, Stream: key, Op: "chat", Position: pos, Want: modelName, Diff: nearMissChat(st.chat, pos)})
	}
	st.chatPos++

	if len(sysInstrJSON) > 0 && ce.promptVersion != "" {
		if live := contentHash(sysInstrJSON); live != ce.promptVersion {
			s.drift = append(s.drift, PromptDrift{Stream: key, Position: pos, Recorded: ce.promptVersion, Live: live})
		}
	}
	return ce.toResponse(), nil
}

// NextToolResult consumes the next recorded execute_tool entry for toolName
// in coords' stream. args is accepted for interface completeness
// (.quack/replay-log.md's matching rule is shallow identity - name only -
// payload bytes are never matched) but not compared. An empty errStr on the
// recorded entry means the call succeeded; a non-empty one is replayed back
// as the call's own error, same as a live failure would be.
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

// NextInvokeAgent consumes the next recorded invoke_agent entry in coords'
// stream, enforcing sequence + shallow identity (agentName - matches
// NextChat's modelName check, redundant with the stream key in practice
// since both derive from the same configured agent name, but keeps the same
// defended-identity shape as every other Next* method). Returns the raw
// ndjson frames both directions of the round exchanged - internal/acp's
// playback path (#604) replays received verbatim and only counts sent.
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

// nearMissAgent builds the near-miss diff for an invoke_agent divergence:
// up to one entry on each side of pos, same shape as nearMissChat.
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

// nearMissTool builds the near-miss diff for a tool-call divergence: the
// other tool names that DO have unconsumed recorded entries in this stream
// (a live call for the wrong tool, or one call too many, both show up as
// "here's what else this stream recorded").
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

// Report returns the session's current divergence accounting: per-stream
// consumed/total, informational prompt drift, and every structural
// MissError returned so far. Safe to call at any point; a clean replay is
// Report().Clean().
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
