package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// Context compaction runs as an ADK BeforeModelCallback (the only hook that sees
// the assembled request), ported to ADK's own durable-event design (ADK Go
// v2.0.0 ships no compaction itself; this follows adk-python's apps/compaction.py
// + flows/llm_flows/contents.go, cross-checked against adk-js — see
// docs/compaction-adk-port-plan.md).
//
// When a request would overflow the window, the callback summarises the older
// turns ONCE and appends the summary as a normal, durably-persisted session
// event (author "model", a sentinel-prefixed text part). Every later request —
// this one and every future turn, for every agent sharing the session — is
// then handled by cheap VIEW-TIME FILTERING: drop everything between the task
// (contents[0], always kept verbatim) and the LAST sentinel content. Raw
// session events are NEVER deleted or mutated; only the per-request view
// shrinks. This is what makes the prompt-cache prefix stable across turns,
// unlike a per-request rewrite.
//
// Invariant: after compaction the model never sees a tool call whose result was
// replaced by a placeholder — a turn is gone (folded into the summary) or intact.
// (A hollowed-out result reads as deleted work, and the model just redoes the call.)
//
// Token counts are estimated as bytes/charsPerToken, then calibrated against the
// provider's measured count from the previous turn — the raw estimate undercounts
// code-dense content and can't see the system prompt + tool schemas.
const (
	charsPerToken = 4

	compactionBuffer    = 20_000 // opencode COMPACTION_BUFFER: max output reserve
	toolOutputMaxChars  = 2_000  // ADK _MAX_TOOL_CONTENT_CHARS / opencode TOOL_OUTPUT_MAX_CHARS: per-tool-content cap when summarising
	minPreserveTokens   = 2_000  // floor for the contents[0] backstop (truncateHeadToFit)
	summaryOutputTokens = 4_096  // opencode SUMMARY_OUTPUT_TOKENS

	maxHeadChars = 120_000 // cap on serialised head fed to the summariser (~30k tokens; safe for a ≥40k-context summariser)

	// defaultEventRetentionSize is ADK's EventsCompactionConfig.event_retention_size
	// default: contents guaranteed to stay verbatim beneath the summary, regardless
	// of how far over threshold the request is. We deliberately do NOT port ADK's
	// sliding-window knobs alongside it (their unit is "invocation"; our runs are
	// single long invocations) — YAGNI.
	defaultEventRetentionSize = 20

	measuredInputKey = "quack.compaction.measured_input" // last provider-reported prompt tokens
	estimateKey      = "quack.compaction.last_estimate"  // raw estimate of the request as sent (paired with the measurement by recordUsage)
	calibrationKey   = "quack.compaction.calibration"    // measured/estimated ratio from the last completed turn
	overheadKey      = "quack.compaction.overhead"       // provider tokens NOT explained by req.Contents (system + tool schemas): ADDITIVE, not a multiplier

	// Calibration ratio bounds. The floor is the default, not 1.0: undercounting
	// is the fatal failure mode (a 400 strands the session; over-compaction only
	// costs context), so a measurement never drags the ratio below the safety
	// margin. The ceiling caps freak ratios from a single skewed turn (tiny
	// request dominated by tool schemas, uncounted media).
	minCalibrationRatio = defaultCalibrationRatio
	maxCalibrationRatio = 8.0
	// First-turn default before any measurement: mildly conservative, because
	// bytes/4 undercounts code and can't see the system-prompt + tool-schema
	// overhead. Compacting a touch early is survivable; overflowing is not.
	defaultCalibrationRatio = 1.3
)

// Compaction carries the per-agent compaction settings into Build.
type Compaction struct {
	Summarizer    model.LLM // summariser model (its own model, opencode runs a hidden agent)
	ContextWindow int       // the agent model's total context window in tokens (0 ⇒ disabled)
	Enabled       bool

	// Sessions is the session service the durable summary event is appended
	// through. Nil disables the durable append: the BeforeModelCallback still
	// filters an already-appended sentinel out of req.Contents (harmless for
	// tests and any standalone caller with no session backing).
	Sessions session.Service
	// TokenThreshold is the trigger budget in tokens. 0 ⇒ derive from
	// ContextWindow via usable().
	TokenThreshold int
	// EventRetentionSize is the minimum number of trailing contents (after the
	// task) that compaction never folds into the summary, regardless of how far
	// over threshold the request is. 0 ⇒ defaultEventRetentionSize.
	EventRetentionSize int
}

// ResolveSummarizer picks which model runs compaction: the active run/node's
// own worker model when one is available (compacting that run's own session
// with a model that's already resident — swap-free by construction), falling
// back to the configured session.compaction.model otherwise (e.g. a
// standalone compaction with no active worker).
func ResolveSummarizer(active, fallback model.LLM) model.LLM {
	if active != nil {
		return active
	}
	return fallback
}

// usable is the input budget: context window minus a fixed output reserve
// (compactionBuffer) left for the model's reply.
func usable(contextWindow int) int {
	if u := contextWindow - compactionBuffer; u > 0 {
		return u
	}
	return 0
}

func (c Compaction) threshold() int {
	if c.TokenThreshold > 0 {
		return c.TokenThreshold
	}
	return usable(c.ContextWindow)
}

func (c Compaction) retention() int {
	if c.EventRetentionSize > 0 {
		return c.EventRetentionSize
	}
	return defaultEventRetentionSize
}

// compactionCallback returns a BeforeModelCallback enforcing the budget. It is a
// no-op whenever the request already fits, which is the common case. The trigger
// prefers the provider's measured prompt-token count from the previous turn
// (recorded by recordUsage) — the chars/4 estimate undercounts dense content and
// miscounts media, so it's only a first-turn fallback before any measurement.
func compactionCallback(c Compaction) llmagent.BeforeModelCallback {
	threshold, retention := c.threshold(), c.retention()
	return func(ctx adkagent.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
		if threshold <= 0 || req == nil {
			return nil, nil
		}
		enforceBudget(ctx, c, threshold, retention, req)
		// Stash the raw estimate of the request AS SENT (post-compaction), so
		// recordUsage can pair it with the provider's measured prompt tokens and
		// refresh the calibration ratio for the next turn.
		if err := ctx.State().Set(estimateKey, estimateTokens(req.Contents)); err != nil {
			slog.Warn("compaction: record estimate", "component", "agent", "err", err)
		}
		return nil, nil
	}
}

// enforceBudget runs the filter → compact → backstop ladder, mutating
// req.Contents in place. Every "did we free enough?" comparison uses the
// CALIBRATED estimate, not raw bytes/4 (see fits).
func enforceBudget(ctx adkagent.Context, c Compaction, threshold, retention int, req *model.LLMRequest) {
	if len(req.Contents) == 0 {
		return
	}
	beforeMsgs, beforeTokens := len(req.Contents), estimateTokens(req.Contents)

	// View-time filtering: ALWAYS runs first, whether or not this round needs a
	// new compaction. A durable summary event appended on a previous turn flows
	// into req.Contents by itself (ADK includes every session event); this drops
	// everything between the task and the LAST such sentinel, at zero summariser
	// cost — the reuse path.
	view := applyView(req.Contents)
	filtered := len(view) != len(req.Contents)
	req.Contents = view

	switch {
	case fits(ctx, threshold, req.Contents):
		if filtered {
			logCompaction(ctx, "filtered_existing", beforeMsgs, beforeTokens, req.Contents, threshold)
		}
	default:
		if headEnd, ok := boundary(req.Contents, retention); ok {
			if out, ok2 := compact(ctx, c, req.Contents, headEnd); ok2 {
				req.Contents = out
				logCompaction(ctx, "event_appended", beforeMsgs, beforeTokens, req.Contents, threshold)
			}
		}
		// else: no self-contained prefix within the retention window (every
		// candidate split lands on an open function call) — skip this round
		// rather than cut a call away from its response (a port of ADK's
		// test_token_threshold_excludes_pending_function_call_events).
	}

	backstop(ctx, req, threshold)
}

// fits reports whether contents is within threshold, preferring the
// provider's last measured prompt-token count over the raw estimate (the
// estimate can't see the system prompt + tool schemas).
func fits(ctx adkagent.Context, threshold int, contents []*genai.Content) bool {
	ratio := calibrationRatio(ctx)
	overhead := intState(ctx, overheadKey)
	return measuredInput(ctx) < threshold && calibrated(estimateTokens(contents), ratio, overhead) <= threshold
}

// logCompaction is the one Info line per applied compaction (#277/#285): a
// compaction silently reshaping the model's context is undiagnosable from the
// outside otherwise. path distinguishes a fresh summariser call
// ("event_appended") from the zero-cost reuse of an already-durable summary
// ("filtered_existing").
func logCompaction(ctx adkagent.Context, path string, beforeMsgs, beforeTokens int, contents []*genai.Content, threshold int) {
	// event_appended is the EXPENSIVE path (a summariser LLM call) — one Info
	// line per real compaction. filtered_existing is the FREE view-filter that
	// runs on EVERY request once a durable summary exists; at Info it emits a
	// line per model call (771 in one run) and reads as thrash when it is a
	// no-cost reuse. Keep it at Debug.
	level := slog.LevelInfo
	if path == "filtered_existing" {
		level = slog.LevelDebug
	}
	slog.Log(ctx, level, "compaction applied", "component", "agent", "path", path,
		"msgs", fmt.Sprintf("%d→%d", beforeMsgs, len(contents)),
		"est_tokens", fmt.Sprintf("%d→%d", beforeTokens, estimateTokens(contents)),
		"threshold", threshold, "session", ctx.SessionID())
}

// backstop runs the last-resort safety net, unchanged in spirit from the
// pre-port implementation: it exists because EventRetentionSize is a count,
// not a token budget, so the retained tail can itself still overflow
// (oversized contents[0], or a tail of colossal tool results — a live grep
// once returned 48 MB). A truncated/clamped result is recoverable — the model
// re-runs the tool narrower; a 400 is not.
func backstop(ctx adkagent.Context, req *model.LLMRequest, threshold int) {
	if len(req.Contents) == 0 {
		return
	}
	ratio := calibrationRatio(ctx)
	overhead := intState(ctx, overheadKey)

	// contents[0] (the task/revise prompt) is never touched by summarisation,
	// so an oversized one is otherwise unrecoverable. Must key on contents[0]'s
	// OWN size, not the whole request's (a ceiling-pinned ratio once shredded a
	// small task that was never too big).
	if head := calibrated(estimateTokens(req.Contents[:1]), ratio, overhead); head > threshold {
		if truncateHeadToFit(req.Contents, threshold, ratio) {
			slog.Warn("compaction truncated an oversized task/revise prompt (contents[0]) as a last resort",
				"component", "agent", "threshold", threshold, "head_tokens", head, "ratio", ratio, "session", ctx.SessionID())
		}
	}
	for _, cap := range []int{8_000, toolOutputMaxChars, 500} {
		if calibrated(estimateTokens(req.Contents), ratio, overhead) <= threshold {
			break
		}
		if n := clampToolResults(req.Contents[1:], cap); n > 0 {
			slog.Warn("compaction clamped oversized tool results in the verbatim tail",
				"component", "agent", "clamped", n, "cap_chars", cap, "session", ctx.SessionID())
		}
	}
	// Compaction is out of moves; a calibrated size still over threshold predicts
	// a hard provider 400 next (which permanently strands the session), so this
	// is worth a Warn even though the request is still sent.
	if got := calibrated(estimateTokens(req.Contents), ratio, overhead); got > threshold {
		slog.Warn("compaction could not bring request under budget; context overflow likely",
			"component", "agent", "calibrated_tokens", got, "threshold", threshold, "ratio", ratio, "session", ctx.SessionID())
	}
}

// clampToolResults middle-elides every tool result in contents whose serialised
// response exceeds maxChars, and reports how many it clamped. The FunctionResponse
// is REPLACED by a same-shaped one carrying the elided text plus a loud marker, so
// the call/response pairing the chat template needs stays intact (a dropped response
// with a live call 400s just as hard as an oversized one).
func clampToolResults(contents []*genai.Content, maxChars int) int {
	clamped := 0
	for _, c := range contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p == nil || p.FunctionResponse == nil {
				continue
			}
			if functionResponseBytes(p.FunctionResponse) <= maxChars {
				continue
			}
			p.FunctionResponse = &genai.FunctionResponse{
				ID:   p.FunctionResponse.ID,
				Name: p.FunctionResponse.Name,
				Response: map[string]any{
					"truncated": true,
					"result":    truncateMiddle(responseText(p.FunctionResponse), maxChars),
					"note": "This result was too large for the context window and its middle was elided. " +
						"Do NOT retry it verbatim — re-run the tool with a narrower query (a more specific path or pattern, " +
						"or exclude build/vendor directories).",
				},
			}
			clamped++
		}
	}
	return clamped
}

// truncateHeadToFit truncates the middle of contents[0] when it alone exceeds
// the budget, leaving room for the verbatim tail. contents[0] is never touched
// by summarisation, so an oversized one is otherwise unrecoverable. Returns
// true if it truncated. Non-text parts (media attachments) are preserved.
func truncateHeadToFit(contents []*genai.Content, budget int, ratio float64) bool {
	if len(contents) == 0 || contents[0] == nil {
		return false
	}
	// Head budget in estimate (bytes/4) space: the real budget converted back
	// through the ratio, minus what the verbatim tail already occupies. Never let
	// it drop below minPreserveTokens so the head keeps a usable core.
	headBudgetEst := int(float64(budget)/ratio) - estimateTokens(contents[1:])
	if headBudgetEst < minPreserveTokens {
		headBudgetEst = minPreserveTokens
	}
	headMaxChars := headBudgetEst * charsPerToken
	if contentBytes(contents[0]) <= headMaxChars {
		return false // the head already fits; the overflow is in the (protected) tail
	}
	contents[0] = truncateContentMiddle(contents[0], headMaxChars)
	return true
}

// truncateContentMiddle collapses c's text parts into one head+marker+tail text
// part fitting maxChars, preserving non-text (media) parts. Middle-out so both
// the opening (instructions) and the closing (acceptance criteria / question)
// of an over-long task survive.
func truncateContentMiddle(c *genai.Content, maxChars int) *genai.Content {
	var text strings.Builder
	var nonText []*genai.Part
	for _, p := range c.Parts {
		switch {
		case p == nil:
		case p.Text != "":
			text.WriteString(p.Text)
		default:
			nonText = append(nonText, p)
		}
	}
	parts := append([]*genai.Part{{Text: truncateMiddle(text.String(), maxChars)}}, nonText...)
	return &genai.Content{Role: c.Role, Parts: parts}
}

// truncateMiddle keeps the head and tail of s around a loud elision marker when
// s exceeds maxChars; otherwise returns s unchanged.
func truncateMiddle(s string, maxChars int) string {
	if len(s) <= maxChars || maxChars <= 0 {
		return s
	}
	const marker = "\n\n…[middle elided to fit the context window — re-read the source files/tools if you need the full detail]…\n\n"
	keep := maxChars - len(marker)
	if keep <= 0 {
		return strings.ToValidUTF8(s[:maxChars], "")
	}
	head := keep / 2
	return strings.ToValidUTF8(s[:head], "") + marker + strings.ToValidUTF8(s[len(s)-(keep-head):], "")
}

// isSentinel reports whether c is a durable compaction summary content: its
// first part's text carries the compactionNotice marker prefix.
func isSentinel(c *genai.Content) bool {
	return c != nil && len(c.Parts) > 0 && strings.HasPrefix(c.Parts[0].Text, compactionNotice)
}

// applyView drops every content strictly between the task (contents[0],
// always kept verbatim — the ADK system-instruction equivalent) and the LAST
// sentinel-marked content in contents. No sentinel found ⇒ contents
// unchanged. This is the whole reuse path: once a summary is durably
// appended, every later request needs no summariser call, only this filter.
func applyView(contents []*genai.Content) []*genai.Content {
	if len(contents) < 3 {
		return contents
	}
	idx := -1
	for i := len(contents) - 1; i >= 1; i-- {
		if isSentinel(contents[i]) {
			idx = i
			break
		}
	}
	if idx <= 1 {
		return contents
	}
	out := make([]*genai.Content, 0, len(contents)-idx+1)
	out = append(out, contents[0])
	return append(out, contents[idx:]...)
}

// boundary returns the index where the new head-to-summarise ends: the
// longest self-contained prefix of contents[1:] within the window bounded by
// retention (the candidate range compaction is allowed to touch). ok is false
// when there's nothing beyond retention worth summarising, or when even the
// window's first content leaves an open function call (no safe cut exists at
// all this round).
func boundary(contents []*genai.Content, retention int) (headEnd int, ok bool) {
	cutCandidate := len(contents) - retention
	if cutCandidate < 2 {
		return 0, false
	}
	n := longestSelfContainedPrefix(contents[1:cutCandidate])
	if n == 0 {
		return 0, false
	}
	return 1 + n, true
}

// longestSelfContainedPrefix returns the length of the longest prefix of
// contents whose FunctionCall/FunctionResponse pairs are fully balanced —
// every call opened within the prefix is closed within it (matched by ID).
// Within one content, responses are applied before calls (a call and its own
// immediate response can share a content). Port of ADK's
// _longest_self_contained_prefix: cutting a call away from its response 400s
// the very next turn, so the compacted range must end where the open set is
// empty — tracked as the last balanced index seen.
func longestSelfContainedPrefix(contents []*genai.Content) int {
	open := map[string]bool{}
	lastBalanced := 0
	for i, c := range contents {
		if c == nil {
			continue
		}
		for _, p := range c.Parts {
			if p != nil && p.FunctionResponse != nil {
				delete(open, p.FunctionResponse.ID)
			}
		}
		for _, p := range c.Parts {
			if p != nil && p.FunctionCall != nil {
				open[p.FunctionCall.ID] = true
			}
		}
		if len(open) == 0 {
			lastBalanced = i + 1
		}
	}
	return lastBalanced
}

// compact summarises contents[1:headEnd] (which may itself contain a prior
// sentinel content, seeding a rolling summary) into ONE new sentinel content,
// durably appends it to the session, and returns the rebuilt view
// [task, sentinel, ...tail]. Returns ok=false (contents unchanged) when there
// is no summariser configured or the summariser call fails — compaction never
// blocks the model call on a failed summarise.
func compact(ctx adkagent.Context, c Compaction, contents []*genai.Content, headEnd int) ([]*genai.Content, bool) {
	if c.Summarizer == nil {
		return contents, false
	}
	head := append([]*genai.Content{}, contents[1:headEnd]...)
	tail := contents[headEnd:]

	prevSummary, rest := extractSentinel(head)
	summary, err := summarizeHead(ctx, c.Summarizer, buildPrompt(prevSummary, serializeHead(rest)))
	if err != nil || strings.TrimSpace(summary) == "" {
		slog.Warn("compaction summarise failed; continuing uncompacted", "component", "agent", "err", err)
		return contents, false
	}

	sentinel := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: compactionNotice + summary}}}
	appendSummaryEvent(ctx, c.Sessions, sentinel)

	out := make([]*genai.Content, 0, 2+len(tail))
	out = append(out, contents[0], sentinel)
	return append(out, tail...), true
}

// extractSentinel pulls the (at most one, since applyView already collapsed
// earlier ones) sentinel content out of contents, returning its summary text
// — the rolling-summary seed for buildPrompt's <previous-summary> — and the
// remaining contents in order. No sentinel present ⇒ ("", contents).
func extractSentinel(contents []*genai.Content) (string, []*genai.Content) {
	for i, c := range contents {
		if isSentinel(c) {
			rest := make([]*genai.Content, 0, len(contents)-1)
			rest = append(rest, contents[:i]...)
			rest = append(rest, contents[i+1:]...)
			return strings.TrimPrefix(c.Parts[0].Text, compactionNotice), rest
		}
	}
	return "", contents
}

// appendSummaryEvent durably persists content as a normal model-authored
// session event, so every future request — this session's, for every agent
// sharing it, on every backend — sees the summary without a per-request
// rewrite (the point of the ADK port: a stable prompt-cache prefix). Fetches
// the session fresh via sessions rather than trusting a cached handle: the
// concrete session services type-assert the exact object THEY vended (see
// e.g. session/database's *localSession assertion), so Get-then-AppendEvent
// on the same service is the only safe sequence. sessions == nil (no session
// backing wired) is a no-op — filtering-only compaction, never a panic.
func appendSummaryEvent(ctx adkagent.Context, sessions session.Service, content *genai.Content) {
	if sessions == nil {
		return
	}
	resp, err := sessions.Get(ctx, &session.GetRequest{AppName: ctx.AppName(), UserID: ctx.UserID(), SessionID: ctx.SessionID()})
	if err != nil || resp == nil || resp.Session == nil {
		slog.Warn("compaction: fetch session for durable summary append", "component", "agent", "err", err, "session", ctx.SessionID())
		return
	}
	ev := session.NewEvent(ctx, ctx.InvocationID())
	// Author = the agent whose callback runs: same-author events are the one
	// class PROVEN to flow back into this agent's next request assembly; an
	// invented author risks exclusion, which would re-compact (and re-append)
	// every turn.
	ev.Author = ctx.AgentName()
	ev.Branch = ctx.Branch()
	ev.Content = content
	if err := sessions.AppendEvent(ctx, resp.Session, ev); err != nil {
		slog.Warn("compaction: append durable summary event", "component", "agent", "err", err, "session", ctx.SessionID())
	}
}

// serializeHead renders the head messages to a plain-text block for the
// summariser: each tool call/result is truncated to toolOutputMaxChars (ADK
// _MAX_TOOL_CONTENT_CHARS / opencode TOOL_OUTPUT_MAX_CHARS), and media parts
// render as a placeholder line rather than vanishing — the summariser must
// know media was there, even though it cannot see it.
func serializeHead(head []*genai.Content) string {
	var sb strings.Builder
	for _, c := range head {
		if c == nil {
			continue
		}
		role := c.Role
		if role == "" {
			role = genai.RoleUser
		}
		for _, p := range c.Parts {
			if p == nil {
				continue
			}
			switch {
			case p.Text != "":
				sb.WriteString(role)
				sb.WriteString(": ")
				sb.WriteString(p.Text)
				sb.WriteByte('\n')
			case p.FunctionCall != nil:
				args, _ := json.Marshal(p.FunctionCall.Args)
				sb.WriteString("[tool call] ")
				sb.WriteString(p.FunctionCall.Name)
				sb.WriteByte('(')
				sb.WriteString(truncate(string(args), toolOutputMaxChars))
				sb.WriteString(")\n")
			case p.FunctionResponse != nil:
				sb.WriteString("[tool result] ")
				sb.WriteString(p.FunctionResponse.Name)
				sb.WriteString(": ")
				sb.WriteString(truncate(responseText(p.FunctionResponse), toolOutputMaxChars))
				sb.WriteByte('\n')
			case p.InlineData != nil:
				sb.WriteString(role)
				sb.WriteString(": [media: ")
				sb.WriteString(p.InlineData.MIMEType)
				sb.WriteString(" omitted]\n")
			case p.FileData != nil:
				sb.WriteString(role)
				sb.WriteString(": [media: ")
				sb.WriteString(p.FileData.MIMEType)
				sb.WriteString(" omitted]\n")
			}
		}
	}
	// Guard the summariser's own context: keep the most recent maxHeadChars
	// (large model text turns aren't truncated above, only tool outputs are).
	s := sb.String()
	if len(s) > maxHeadChars {
		s = "[…older history truncated…]\n" + strings.ToValidUTF8(s[len(s)-maxHeadChars:], "")
	}
	return s
}

// buildPrompt assembles the summariser user message (opencode buildPrompt):
// history first, then the create/update instruction, then the output template.
func buildPrompt(previousSummary, head string) string {
	lead := "Create a new summary from the conversation history."
	if strings.TrimSpace(previousSummary) != "" {
		lead = "Update the summary below using the conversation history above.\n" +
			"Preserve still-true details, remove stale details, and merge in the new facts.\n" +
			"<previous-summary>\n" + previousSummary + "\n</previous-summary>"
	}
	return strings.Join([]string{head, lead, summaryTemplate}, "\n\n")
}

// summarizeHead runs the summariser model once and returns its text.
func summarizeHead(ctx context.Context, summarizer model.LLM, prompt string) (string, error) {
	req := &model.LLMRequest{
		Contents: []*genai.Content{{
			Role:  genai.RoleUser,
			Parts: []*genai.Part{{Text: compactionSystemPrompt + "\n\n" + prompt}},
		}},
		Config: &genai.GenerateContentConfig{MaxOutputTokens: summaryOutputTokens},
	}
	var out strings.Builder
	var lastErr error
	for resp, err := range summarizer.GenerateContent(ctx, req, false) {
		if err != nil {
			lastErr = err
			continue
		}
		if resp == nil || resp.Content == nil {
			continue
		}
		for _, p := range resp.Content.Parts {
			out.WriteString(p.Text)
		}
	}
	if out.Len() == 0 && lastErr != nil {
		return "", lastErr
	}
	return out.String(), nil
}

// recordUsage is an AfterModelCallback that stashes the provider's measured
// prompt-token count so the next BeforeModelCallback can trigger on real usage
// rather than the chars/4 estimate (the OpenCode approach: measure, then compact
// before the next turn). The model itself is unchanged — we return (nil, nil).
func recordUsage() llmagent.AfterModelCallback {
	return func(ctx adkagent.Context, resp *model.LLMResponse, err error) (*model.LLMResponse, error) {
		if err != nil || resp == nil || resp.UsageMetadata == nil || resp.UsageMetadata.PromptTokenCount <= 0 {
			return nil, nil
		}
		measured := int(resp.UsageMetadata.PromptTokenCount)
		if e := ctx.State().Set(measuredInputKey, measured); e != nil {
			slog.Warn("compaction: record usage", "component", "agent", "err", e)
		}
		// Calibrate: solve measured ≈ overhead + density×estimate for the
		// OVERHEAD, holding density at its default — the overhead is the part
		// that is actually fixed, so it generalises to the next turn.
		if est := intState(ctx, estimateKey); est > 0 {
			overhead := measured - int(float64(est)*defaultCalibrationRatio)
			if overhead < 0 {
				overhead = 0
			}
			if e := ctx.State().Set(overheadKey, overhead); e != nil {
				slog.Warn("compaction: record overhead", "component", "agent", "err", e)
			}
		}
		return nil, nil
	}
}

// measuredInput returns the last provider-reported prompt-token count, or 0.
func measuredInput(ctx adkagent.Context) int { return intState(ctx, measuredInputKey) }

// calibrationRatio returns the measured/estimated prompt-token ratio recorded
// from the last completed turn, clamped, or defaultCalibrationRatio before any
// measurement exists.
func calibrationRatio(ctx adkagent.Context) float64 {
	v, err := ctx.State().Get(calibrationKey)
	if err != nil {
		return defaultCalibrationRatio
	}
	switch r := v.(type) {
	case float64:
		return clampRatio(r)
	case int: // tolerate an int round-trip, as intState does
		return clampRatio(float64(r))
	default:
		return defaultCalibrationRatio
	}
}

func clampRatio(r float64) float64 {
	return min(max(r, minCalibrationRatio), maxCalibrationRatio)
}

// calibrated scales a raw bytes/4 estimate into approximate real prompt tokens:
//
//	measured ≈ overhead + density × estimate(req.Contents)
//
// The overhead (system instruction + tool schemas) is FIXED and invisible to
// estimateTokens, so it must be additive — folding it into a multiplier blows up
// exactly when the content is small.
func calibrated(estimate int, density float64, overhead int) int {
	return overhead + int(float64(estimate)*density)
}

// intState reads an int from session state, tolerating the float64 a JSON-backed
// (DB) session round-trips through.
func intState(ctx adkagent.Context, key string) int {
	v, err := ctx.State().Get(key)
	if err != nil {
		return 0
	}
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	default:
		return 0
	}
}

// --- size estimation -------------------------------------------------------

func estimateTokens(contents []*genai.Content) int {
	total := 0
	for _, c := range contents {
		total += contentBytes(c)
	}
	return total / charsPerToken
}

func contentBytes(c *genai.Content) int {
	if c == nil {
		return 0
	}
	n := 0
	for _, p := range c.Parts {
		n += partBytes(p)
	}
	return n
}

func partBytes(p *genai.Part) int {
	if p == nil {
		return 0
	}
	n := len(p.Text)
	if p.FunctionCall != nil {
		n += len(p.FunctionCall.Name) + mapBytes(p.FunctionCall.Args)
	}
	if p.FunctionResponse != nil {
		n += functionResponseBytes(p.FunctionResponse)
	}
	// InlineData/FileData (media) are intentionally NOT counted: a vision model
	// bills an image at a few hundred tokens, not its raw byte length. Counting
	// bytes made the estimate fire compaction on every image turn (the measured
	// usage from recordUsage is the real budget signal for media).
	return n
}

func functionResponseBytes(fr *genai.FunctionResponse) int {
	return len(fr.Name) + mapBytes(fr.Response) // media parts excluded, as in partBytes
}

func mapBytes(m map[string]any) int {
	if len(m) == 0 {
		return 0
	}
	b, err := json.Marshal(m)
	if err != nil {
		return 0
	}
	return len(b)
}

// responseText joins the string values of a tool response for serialisation.
func responseText(fr *genai.FunctionResponse) string {
	var sb strings.Builder
	for _, v := range fr.Response {
		if s, ok := v.(string); ok {
			sb.WriteString(s)
		}
	}
	if sb.Len() == 0 {
		b, _ := json.Marshal(fr.Response)
		return string(b)
	}
	return sb.String()
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.ToValidUTF8(s[:max], "") + "…"
}
