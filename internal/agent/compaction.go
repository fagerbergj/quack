package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/stream"
)

// Compaction summarises older turns into a durable sentinel event, then filters
// the per-request view cheaply on every later turn. Raw events are never deleted.
// Token estimates use bytes/charsPerToken calibrated against provider measurements.
const (
	charsPerToken = 4

	compactionBuffer    = 20_000
	toolOutputMaxChars  = 2_000
	minPreserveTokens   = 2_000
	summaryOutputTokens = 4_096

	defaultEventRetentionSize = 20

	measuredInputKey           = "quack.compaction.measured_input"
	estimateKey                = "quack.compaction.last_estimate"
	overheadKey                = "quack.compaction.overhead"
	invocationCountKey         = "quack.compaction.invocation_count"
	lastInvocationIDKey        = "quack.compaction.last_invocation_id"
	lastCompactedInvocationKey = "quack.compaction.last_compacted_invocation"

	defaultCalibrationRatio = 1.3
)

type Compaction struct {
	Summarizer         model.LLM
	ContextWindow      int
	Enabled            bool
	Sessions           session.Service
	TokenThreshold     int
	EventRetentionSize int
	// CompactionInterval is ADK's regular-cadence trigger (in invocations),
	// on top of TokenThreshold's absolute limit. 0 disables the cadence trigger.
	CompactionInterval int
	// OverlapSize is how many already-windowed raw events carry into the next
	// summarization pass, so a fact split across the cut isn't lost. 0 = default.
	OverlapSize int
}

// ResolveSummarizer prefers the active worker model for compaction (swap-free), falling back to the configured one.
func ResolveSummarizer(active, fallback model.LLM) model.LLM {
	if active != nil {
		return active
	}
	return fallback
}

// usable is the input budget: context window minus output reserve.
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

// overlap is opt-in: 0 (unset) preserves the pre-ADK-port behaviour of
// folding the whole window into the summary every time.
func (c Compaction) overlap() int {
	return c.OverlapSize
}

// intervalTrigger reports whether the invocation cadence (ADK's
// compaction_interval) is due, advancing the per-session invocation counter
// at most once per distinct invocation ID and firing at most once per
// invocation that lands on the interval.
func intervalTrigger(ctx adkagent.Context, c Compaction) bool {
	interval := c.CompactionInterval
	if interval <= 0 {
		return false
	}
	cur := ctx.InvocationID()
	if last, err := ctx.State().Get(lastInvocationIDKey); err != nil || last != cur {
		if e := ctx.State().Set(invocationCountKey, intState(ctx, invocationCountKey)+1); e != nil {
			slog.Warn("compaction: record invocation count", "component", "agent", "err", e)
		}
		if e := ctx.State().Set(lastInvocationIDKey, cur); e != nil {
			slog.Warn("compaction: record invocation id", "component", "agent", "err", e)
		}
	}
	count := intState(ctx, invocationCountKey)
	if count == 0 || count%interval != 0 || intState(ctx, lastCompactedInvocationKey) == count {
		return false
	}
	if e := ctx.State().Set(lastCompactedInvocationKey, count); e != nil {
		slog.Warn("compaction: record last compacted invocation", "component", "agent", "err", e)
	}
	return true
}

// compactionCallback returns a BeforeModelCallback that enforces the budget.
func compactionCallback(c Compaction) llmagent.BeforeModelCallback {
	threshold, retention := c.threshold(), c.retention()
	return func(ctx adkagent.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
		if threshold <= 0 || req == nil {
			return nil, nil
		}
		enforceBudget(ctx, c, threshold, retention, req)
		if err := ctx.State().Set(estimateKey, estimateTokens(req.Contents)); err != nil {
			slog.Warn("compaction: record estimate", "component", "agent", "err", err)
		}
		return nil, nil
	}
}

// enforceBudget runs filter → compact → backstop, mutating req.Contents in place.
func enforceBudget(ctx adkagent.Context, c Compaction, threshold, retention int, req *model.LLMRequest) {
	if len(req.Contents) == 0 {
		return
	}
	beforeMsgs, beforeTokens := len(req.Contents), estimateTokens(req.Contents)

	view := applyView(req.Contents)
	filtered := len(view) != len(req.Contents)
	req.Contents = view

	// Two independent triggers, as in ADK: token_threshold is the absolute
	// safety limit, compaction_interval is the regular cadence regardless of
	// size. Either alone is enough to fire a compaction round.
	due := intervalTrigger(ctx, c)
	exceeds := !fits(ctx, threshold, req.Contents)

	switch {
	case !exceeds && !due:
		if filtered {
			logCompaction(ctx, "filtered_existing", beforeMsgs, beforeTokens, req.Contents, threshold)
		}
	default:
		if headEnd, ok := boundary(req.Contents, retention); ok {
			if out, ok2 := compact(ctx, c, req.Contents, headEnd); ok2 {
				req.Contents = out
				path := "event_appended"
				if !exceeds {
					path = "interval_appended"
				}
				logCompaction(ctx, path, beforeMsgs, beforeTokens, req.Contents, threshold)
				emitCompaction(ctx, beforeTokens, estimateTokens(req.Contents))
			}
		}
	}

	backstop(ctx, req, threshold)
}

// fits reports whether contents is within threshold using calibrated estimates.
func fits(ctx adkagent.Context, threshold int, contents []*genai.Content) bool {
	ratio := defaultCalibrationRatio
	overhead := intState(ctx, overheadKey)
	return measuredInput(ctx) < threshold && calibrated(estimateTokens(contents), ratio, overhead) <= threshold
}

// logCompaction is the one log line per applied compaction.
func logCompaction(ctx adkagent.Context, path string, beforeMsgs, beforeTokens int, contents []*genai.Content, threshold int) {
	level := slog.LevelInfo
	if path == "filtered_existing" {
		level = slog.LevelDebug
	}
	slog.Log(ctx, level, "compaction applied", "component", "agent", "path", path,
		"msgs", fmt.Sprintf("%d→%d", beforeMsgs, len(contents)),
		"est_tokens", fmt.Sprintf("%d→%d", beforeTokens, estimateTokens(contents)),
		"threshold", threshold, "session", ctx.SessionID())
}

// emitCompaction forwards a node-scoped compaction event to the SSE stream,
// via the same yield-ctx escape hatch the plan tool uses - a no-op outside a
// DAG node run (e.g. unit tests, or the advisor's own nested runner).
//
// The round's own OTel span is unreachable from here: RunNode rebuilds the
// child context this callback runs under (the SetLedgerCoords-not-ctx pattern
// elsewhere in this codebase exists for the same reason), so
// oteltrace.SpanFromContext(ctx) would silently return the no-op span. Coords
// carries the round's SpanContext captured before that rebuild
// (runWorkerNodeTraced); re-inject it so this span parents under the round
// instead of rooting a disconnected trace. A zero SpanContext (no linkage
// available) still raises an orphan span rather than none at all - do NOT
// use StartLinked here, it forces WithNewRoot and would still fork the trace.
func emitCompaction(ctx adkagent.Context, before, after int) {
	coords := ledger.CoordsFromContext(ctx)
	parentCtx := oteltrace.ContextWithSpanContext(ctx, coords.SpanContext)
	_, span := otelobs.Start(parentCtx, "compaction",
		attribute.String("node_id", coords.Node), attribute.String("run_id", coords.Round),
		attribute.Bool(otelobs.GenAIConversationCompacted, true))
	span.End()

	sink, ok := stream.YieldFromContext(ctx)
	if !ok {
		return
	}
	sink(stream.Compaction(coords.Node, coords.Round, int32(before), int32(after)))
}

// backstop handles overflow of the verbatim tail.
func backstop(ctx adkagent.Context, req *model.LLMRequest, threshold int) {
	if len(req.Contents) == 0 {
		return
	}
	ratio := defaultCalibrationRatio
	overhead := intState(ctx, overheadKey)

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
	if got := calibrated(estimateTokens(req.Contents), ratio, overhead); got > threshold {
		slog.Warn("compaction could not bring request under budget; context overflow likely",
			"component", "agent", "calibrated_tokens", got, "threshold", threshold, "ratio", ratio, "session", ctx.SessionID())
	}
}

// clampToolResults middle-elides oversized tool results in contents.
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
						"Do NOT retry it verbatim - re-run the tool with a narrower query (a more specific path or pattern, " +
						"or exclude build/vendor directories).",
				},
			}
			clamped++
		}
	}
	return clamped
}

// truncateHeadToFit truncates contents[0]'s middle when it alone overflows the budget.
func truncateHeadToFit(contents []*genai.Content, budget int, ratio float64) bool {
	if len(contents) == 0 || contents[0] == nil {
		return false
	}
	headBudgetEst := int(float64(budget)/ratio) - estimateTokens(contents[1:])
	if headBudgetEst < minPreserveTokens {
		headBudgetEst = minPreserveTokens
	}
	headMaxChars := headBudgetEst * charsPerToken
	if contentBytes(contents[0]) <= headMaxChars {
		return false
	}
	contents[0] = truncateContentMiddle(contents[0], headMaxChars)
	return true
}

// truncateContentMiddle collapses text parts into one head+marker+tail, preserving media parts.
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

// truncateMiddle keeps head and tail around an elision marker.
func truncateMiddle(s string, maxChars int) string {
	if len(s) <= maxChars || maxChars <= 0 {
		return s
	}
	const marker = "\n\n…[middle elided to fit the context window - re-read the source files/tools if you need the full detail]…\n\n"
	keep := maxChars - len(marker)
	if keep <= 0 {
		return strings.ToValidUTF8(s[:maxChars], "")
	}
	head := keep / 2
	return strings.ToValidUTF8(s[:head], "") + marker + strings.ToValidUTF8(s[len(s)-(keep-head):], "")
}

// isSentinel reports whether c carries the durable compaction summary marker.
func isSentinel(c *genai.Content) bool {
	return c != nil && len(c.Parts) > 0 && strings.HasPrefix(c.Parts[0].Text, compactionNotice)
}

// applyView drops everything between contents[0] and the last sentinel.
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

// boundary finds the longest self-contained prefix within the retention window.
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

// longestSelfContainedPrefix finds the longest prefix with balanced FunctionCall/FunctionResponse pairs.
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

// compact summarises contents[1:headEnd] into a durable sentinel, returning
// [task, sentinel, ...overlap, ...tail]. The last overlap() events of the
// window are kept raw rather than folded in now (ADK's overlap_size): they
// stay visible verbatim and are re-offered to the summariser next round, so a
// fact that lands right at a chunk boundary is never seen only once.
func compact(ctx adkagent.Context, c Compaction, contents []*genai.Content, headEnd int) ([]*genai.Content, bool) {
	if c.Summarizer == nil {
		return contents, false
	}
	head := append([]*genai.Content{}, contents[1:headEnd]...)
	tail := contents[headEnd:]

	prevSummary, rest := extractSentinel(head)

	overlap := c.overlap()
	toSummarize, keep := rest, []*genai.Content(nil)
	if overlap > 0 {
		// ADK clamps overlap to what exists; never leave nothing to summarise.
		if overlap >= len(rest) {
			overlap = len(rest) - 1
		}
		if overlap > 0 {
			toSummarize, keep = rest[:len(rest)-overlap], rest[len(rest)-overlap:]
		}
	}
	if len(toSummarize) == 0 {
		return contents, false
	}

	budgetChars := usable(c.ContextWindow) * charsPerToken
	summary, err := summarizeChunks(ctx, c.Summarizer, prevSummary, chunkByBudget(toSummarize, budgetChars))
	if err != nil || strings.TrimSpace(summary) == "" {
		slog.Warn("compaction summarise failed; continuing uncompacted", "component", "agent", "err", err)
		return contents, false
	}

	sentinel := &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: compactionNotice + summary}}}
	appendSummaryEvent(ctx, c.Sessions, sentinel)

	out := make([]*genai.Content, 0, 2+len(keep)+len(tail))
	out = append(out, contents[0], sentinel)
	out = append(out, keep...)
	return append(out, tail...), true
}

// chunkByBudget splits head into ordered sub-slices whose serialized text
// each fits budgetChars, cut at balanced tool-call boundaries where possible.
// No hard truncation: an oversized single event still gets its own chunk.
func chunkByBudget(head []*genai.Content, budgetChars int) [][]*genai.Content {
	if budgetChars <= 0 || len(head) == 0 {
		return [][]*genai.Content{head}
	}
	var chunks [][]*genai.Content
	start := 0
	for start < len(head) {
		end, size := start+1, contentBytes(head[start])
		for end < len(head) {
			next := contentBytes(head[end])
			if size+next > budgetChars {
				break
			}
			size += next
			end++
		}
		if bal := start + longestSelfContainedPrefix(head[start:end]); bal > start {
			end = bal
		}
		chunks = append(chunks, head[start:end])
		start = end
	}
	return chunks
}

// summarizeChunks folds chunks into a running summary, iterating rather than
// truncating when a window exceeds the summariser's input budget.
func summarizeChunks(ctx context.Context, summarizer model.LLM, prevSummary string, chunks [][]*genai.Content) (string, error) {
	running := prevSummary
	for _, chunk := range chunks {
		s, err := summarizeHead(ctx, summarizer, buildPrompt(running, serializeHead(chunk)))
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(s) != "" {
			running = s
		}
	}
	return running, nil
}

// extractSentinel returns the sentinel's summary text and the remaining contents.
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

// appendSummaryEvent durably persists content as a model-authored session event.
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
	ev.Author = ctx.AgentName()
	ev.Branch = ctx.Branch()
	ev.Content = content
	if err := sessions.AppendEvent(ctx, resp.Session, ev); err != nil {
		slog.Warn("compaction: append durable summary event", "component", "agent", "err", err, "session", ctx.SessionID())
	}
}

// serializeHead renders head messages as plain text for the summariser.
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
	return sb.String()
}

// buildPrompt assembles the summariser user message.
func buildPrompt(previousSummary, head string) string {
	lead := "Create a new summary from the conversation history."
	if strings.TrimSpace(previousSummary) != "" {
		lead = "Update the summary below using the conversation history above.\n" +
			"Preserve still-true details, remove stale details, and merge in the new facts.\n" +
			"<previous-summary>\n" + previousSummary + "\n</previous-summary>"
	}
	return strings.Join([]string{head, lead, summaryTemplate}, "\n\n")
}

// summarizeHead runs the summariser model once.
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

// recordUsage stashes the provider's measured prompt-token count for the next compaction trigger.
func recordUsage() llmagent.AfterModelCallback {
	return func(ctx adkagent.Context, resp *model.LLMResponse, err error) (*model.LLMResponse, error) {
		if err != nil || resp == nil || resp.UsageMetadata == nil || resp.UsageMetadata.PromptTokenCount <= 0 {
			return nil, nil
		}
		measured := int(resp.UsageMetadata.PromptTokenCount)
		if e := ctx.State().Set(measuredInputKey, measured); e != nil {
			slog.Warn("compaction: record usage", "component", "agent", "err", e)
		}
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

// measuredInput returns the last provider-reported prompt-token count.
func measuredInput(ctx adkagent.Context) int { return intState(ctx, measuredInputKey) }

// calibrated scales a raw estimate into approximate real prompt tokens.
// Overhead is additive (system + tool schemas), not a multiplier.
func calibrated(estimate int, density float64, overhead int) int {
	return overhead + int(float64(estimate)*density)
}

// intState reads an int from session state, tolerating JSON-backed float64 round-trips.
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

// --- helpers -------------------------------------------------------

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
