package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// Context compaction ports sst/opencode's two-stage strategy into an ADK
// BeforeModelCallback, the only v1.4.0 hook that sees the assembled request
// before it hits the model. The ADK runner rebuilds every request from the full
// session history with no trimming, so a node's worker session (worker tool loop
// + the trust gate's self-critique / revision rounds, all in one session) grows
// past the model's window on large research runs. The callback fires before each
// model call and, when the request would overflow:
//
//  1. prune  — blank old tool-output payloads (cheap, no model call).
//  2. compact — if still over budget, summarise the older turns via a separate
//     summariser model into an anchored summary kept in session state.
//
// Token counts are estimated as bytes/charsPerToken, then CALIBRATED by the
// measured/estimated ratio of the previous turn (see calibrationRatio): the raw
// estimate undercounts code-dense content (~3 chars/token or worse for
// symbols/JSON) and cannot see the system instruction + tool declarations the
// framework appends outside req.Contents, which together made the callback
// declare victory at an estimated ~40k while the provider counted 66k and
// 400'd. An exact tokenizer round trip per model turn isn't worth it (see
// [llama-swap tokenize endpoint] if it ever needs to be exact).
const (
	charsPerToken = 4

	compactionBuffer    = 20_000 // opencode COMPACTION_BUFFER: max output reserve
	pruneProtectTokens  = 40_000 // opencode PRUNE_PROTECT: recent tool output kept verbatim
	pruneMinimumTokens  = 20_000 // opencode PRUNE_MINIMUM: don't prune unless it frees this much
	tailTurns           = 2      // opencode DEFAULT_TAIL_TURNS: trailing messages kept verbatim
	toolOutputMaxChars  = 2_000  // opencode TOOL_OUTPUT_MAX_CHARS: per-tool-output cap when summarising
	minPreserveTokens   = 2_000  // opencode MIN_PRESERVE_RECENT_TOKENS
	maxPreserveTokens   = 8_000  // opencode MAX_PRESERVE_RECENT_TOKENS
	summaryOutputTokens = 4_096  // opencode SUMMARY_OUTPUT_TOKENS

	maxHeadChars = 120_000 // cap on serialised head fed to the summariser (~30k tokens; safe for a ≥40k-context summariser)

	summaryStateKey  = "quack.compaction.summary"
	summaryCoversKey = "quack.compaction.summary_covers" // # leading messages the summary folds in
	measuredInputKey = "quack.compaction.measured_input" // last provider-reported prompt tokens
	estimateKey      = "quack.compaction.last_estimate"  // raw estimate of the request as sent (paired with the measurement by recordUsage)
	calibrationKey   = "quack.compaction.calibration"    // measured/estimated ratio from the last completed turn
	prunedStub       = "[earlier tool output elided to fit the context window]"

	// Calibration ratio bounds. The floor is 1.0 because calibration must never
	// make the estimate SMALLER than raw bytes/4 — undercounting is the failure
	// mode (a provider 400 kills the node's session for good; over-compaction
	// merely costs context). The ceiling caps freak ratios (a tiny request whose
	// measurement is dominated by tool schemas, or a media turn the estimate
	// doesn't count) so a single skewed turn can't force pathological
	// over-compaction forever — the ratio is re-measured every turn anyway.
	minCalibrationRatio = 1.0
	maxCalibrationRatio = 8.0
	// Before any measurement exists (first model call of a session) we apply a
	// mildly conservative default rather than trusting bytes/4: chars/4 already
	// runs slightly under on prose and badly under on code, and the first
	// request carries the full unseen system-prompt + tool-schema overhead.
	// Triggering compaction a touch early is survivable; overflowing is not.
	defaultCalibrationRatio = 1.3
)

// Compaction carries the per-agent compaction settings into Build.
type Compaction struct {
	Summarizer    model.LLM // summariser model (its own model, opencode runs a hidden agent)
	ContextWindow int       // the agent model's total context window in tokens (0 ⇒ disabled)
	Prune         bool      // run the cheap tool-output prune pass before summarising
	Enabled       bool
}

// usable is the input budget: context window minus a fixed output reserve
// (compactionBuffer) left for the model's reply.
func usable(contextWindow int) int {
	if u := contextWindow - compactionBuffer; u > 0 {
		return u
	}
	return 0
}

// compactionCallback returns a BeforeModelCallback enforcing the budget. It is a
// no-op whenever the request already fits, which is the common case. The trigger
// prefers the provider's measured prompt-token count from the previous turn
// (recorded by recordUsage) — the chars/4 estimate undercounts dense content and
// miscounts media, so it's only a first-turn fallback before any measurement.
func compactionCallback(c Compaction) llmagent.BeforeModelCallback {
	budget := usable(c.ContextWindow)
	return func(ctx adkagent.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
		if budget <= 0 || req == nil {
			return nil, nil
		}
		enforceBudget(ctx, c, budget, req)
		// Stash the raw estimate of the request AS SENT (post-compaction), so
		// recordUsage can pair it with the provider's measured prompt tokens and
		// refresh the calibration ratio for the next turn.
		if err := ctx.State().Set(estimateKey, estimateTokens(req.Contents)); err != nil {
			slog.Warn("compaction: record estimate", "component", "agent", "err", err)
		}
		return nil, nil
	}
}

// enforceBudget runs the prune → reuse-summary → summarise ladder, mutating
// req.Contents in place. Every "did we free enough?" comparison uses the
// CALIBRATED estimate, not raw bytes/4: the ratio folds in both the true
// tokenizer density of the session's content and the fixed request overhead
// (system instruction + tool declarations) that estimateTokens can't see —
// there's no need to model the overhead separately.
func enforceBudget(ctx adkagent.Context, c Compaction, budget int, req *model.LLMRequest) {
	ratio := calibrationRatio(ctx)
	if measuredInput(ctx) < budget && calibrated(estimateTokens(req.Contents), ratio) <= budget {
		return
	}
	if c.Prune {
		if freed := prune(req.Contents); freed > 0 {
			slog.Debug("compaction pruned tokens", "component", "agent", "freed", freed, "session", ctx.SessionID())
		}
		if calibrated(estimateTokens(req.Contents), ratio) <= budget {
			return
		}
	}
	// Reuse fast-path: session events are append-only, so the summary's
	// coverage boundary is a stable index. If the anchored summary plus the
	// live tail since it still fits, reapply it with NO summariser call — only
	// re-summarise when that tail has itself grown past budget.
	if prev, n := readSummary(ctx); prev != "" && n > 1 && n <= len(req.Contents) {
		reused := append([]*genai.Content{mergeTaskSummary(req.Contents[0], prev)}, req.Contents[n:]...)
		if calibrated(estimateTokens(reused), ratio) <= budget {
			req.Contents = reused
			slog.Debug("compaction reused anchored summary; no summariser call", "component", "agent", "covers_msgs", n, "session", ctx.SessionID())
			return
		}
	}
	// preserve is budgeted in real tokens (budget is real), but splitHead sums
	// raw bytes/4 sizes — convert to estimate-space by dividing by the ratio so
	// the preserved tail doesn't overflow the budget once calibrated back.
	preserve := int(float64(min(max(budget/4, minPreserveTokens), maxPreserveTokens)) / ratio)
	if out, ok := compact(ctx, c.Summarizer, req.Contents, preserve); ok {
		req.Contents = out
		slog.Debug("compaction summarised head", "component", "agent", "tokens_now", estimateTokens(req.Contents), "session", ctx.SessionID())
	}
	// Compaction is out of moves; a calibrated size still over budget predicts a
	// hard provider 400 next (which permanently strands the session), so this is
	// worth a Warn even though the request is still sent.
	if got := calibrated(estimateTokens(req.Contents), ratio); got > budget {
		slog.Warn("compaction could not bring request under budget; context overflow likely",
			"component", "agent", "calibrated_tokens", got, "budget", budget, "ratio", ratio, "session", ctx.SessionID())
	}
}

// prune blanks the payloads of old FunctionResponse parts (tool outputs the
// model has already consumed), protecting the most recent pruneProtectTokens of
// tool output plus the last tailTurns messages. Call↔response pairing is kept
// intact: only the response body is replaced, ID/Name preserved. Returns the
// estimated tokens freed, and applies nothing unless that exceeds
// pruneMinimumTokens (so a tiny win isn't worth the lost context).
func prune(contents []*genai.Content) int {
	var targets []*genai.FunctionResponse
	kept, freed := 0, 0
	for ci := len(contents) - 1; ci >= 0; ci-- {
		c := contents[ci]
		if c == nil {
			continue
		}
		protected := ci >= len(contents)-tailTurns
		for _, p := range c.Parts {
			fr := p.FunctionResponse
			if fr == nil {
				continue
			}
			sz := functionResponseBytes(fr) / charsPerToken
			if protected || kept < pruneProtectTokens {
				kept += sz
				continue
			}
			targets = append(targets, fr)
			freed += sz - len(prunedStub)/charsPerToken
		}
	}
	if freed <= pruneMinimumTokens {
		return 0
	}
	for _, fr := range targets {
		fr.Response = map[string]any{"result": prunedStub}
		fr.Parts = nil
	}
	return freed
}

// compact summarises the head (older turns) into an anchored summary and rebuilds
// contents as [task, summary, ...tail]. contents[0] (the self-contained task) and
// the recent tail are kept verbatim. Returns ok=false (contents unchanged) when
// there's no head worth summarising or the summariser call fails.
func compact(ctx adkagent.Context, summarizer model.LLM, contents []*genai.Content, preserve int) ([]*genai.Content, bool) {
	if summarizer == nil {
		return contents, false
	}
	tailStart := splitHead(contents, preserve)
	if tailStart <= 1 {
		return contents, false // nothing older than the task to summarise
	}
	head, tail := contents[1:tailStart], contents[tailStart:]

	prev, _ := readSummary(ctx)
	summary, err := summarizeHead(ctx, summarizer, buildPrompt(prev, serializeHead(head)))
	if err != nil || strings.TrimSpace(summary) == "" {
		slog.Warn("compaction summarise failed; continuing uncompacted", "component", "agent", "err", err)
		return contents, false
	}
	writeSummary(ctx, summary, tailStart)

	out := make([]*genai.Content, 0, 1+len(tail))
	out = append(out, mergeTaskSummary(contents[0], summary))
	return append(out, tail...), true
}

// mergeTaskSummary appends the anchored summary to the task content rather than
// inserting a separate turn: a standalone summary message could sit adjacent to
// another same-role turn and trip strict chat templates. The task stays Parts[0].
func mergeTaskSummary(task *genai.Content, summary string) *genai.Content {
	return &genai.Content{
		Role:  task.Role,
		Parts: append(append([]*genai.Part{}, task.Parts...), &genai.Part{Text: "\n\nSummary of earlier work (older turns were compacted):\n" + summary}),
	}
}

// splitHead returns the index where the verbatim tail begins. contents[0] is
// always kept; [1:tailStart] is the head; [tailStart:] is the tail. The tail
// grows backward from the end until it would exceed preserve tokens. If the
// boundary lands on a dangling FunctionResponse (whose matching call would be
// summarised away in the head), it is moved EARLIER to pull the call into the
// tail — never later, which would drop the recent tool result the model is about
// to act on. If that walks back to the task, there's no head and we don't compact.
func splitHead(contents []*genai.Content, preserve int) int {
	if len(contents) <= 2 {
		return len(contents)
	}
	tailStart := len(contents)
	used := 0
	for ci := len(contents) - 1; ci >= 1; ci-- {
		ct := contentBytes(contents[ci]) / charsPerToken
		if used+ct > preserve && tailStart < len(contents) {
			break
		}
		used += ct
		tailStart = ci
	}
	for tailStart > 1 && tailStart < len(contents) && hasFunctionResponse(contents[tailStart]) {
		tailStart--
	}
	return tailStart
}

// serializeHead renders the head messages to a plain-text block for the
// summariser: media is dropped and each tool output is truncated to
// toolOutputMaxChars (opencode stripMedia + toolOutputMaxChars).
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
				sb.Write(args)
				sb.WriteString(")\n")
			case p.FunctionResponse != nil:
				sb.WriteString("[tool result] ")
				sb.WriteString(p.FunctionResponse.Name)
				sb.WriteString(": ")
				sb.WriteString(truncate(responseText(p.FunctionResponse), toolOutputMaxChars))
				sb.WriteByte('\n')
			}
			// InlineData / FileData (media) intentionally skipped.
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
	lead := "Create a new anchored summary from the conversation history."
	if strings.TrimSpace(previousSummary) != "" {
		lead = "Update the anchored summary below using the conversation history above.\n" +
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
		// Calibrate the estimator: the BeforeModelCallback stashed the raw
		// estimate for this same request, so measured/estimate is the true
		// correction factor for this session's content mix + fixed overhead.
		if est := intState(ctx, estimateKey); est > 0 {
			ratio := clampRatio(float64(measured) / float64(est))
			if e := ctx.State().Set(calibrationKey, ratio); e != nil {
				slog.Warn("compaction: record calibration", "component", "agent", "err", e)
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

// calibrated scales a raw bytes/4 estimate into approximate real prompt tokens.
func calibrated(estimate int, ratio float64) int {
	return int(float64(estimate) * ratio)
}

// readSummary returns the anchored summary and how many leading messages it folds
// in (0 if none yet).
func readSummary(ctx adkagent.Context) (string, int) {
	s := ""
	if v, err := ctx.State().Get(summaryStateKey); err == nil {
		s, _ = v.(string)
	}
	return s, intState(ctx, summaryCoversKey)
}

func writeSummary(ctx adkagent.Context, s string, coversN int) {
	if err := ctx.State().Set(summaryStateKey, s); err != nil {
		slog.Warn("compaction: persist summary", "component", "agent", "err", err)
	}
	if err := ctx.State().Set(summaryCoversKey, coversN); err != nil {
		slog.Warn("compaction: persist summary coverage", "component", "agent", "err", err)
	}
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

func hasFunctionResponse(c *genai.Content) bool {
	if c == nil {
		return false
	}
	for _, p := range c.Parts {
		if p.FunctionResponse != nil {
			return true
		}
	}
	return false
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return strings.ToValidUTF8(s[:max], "") + "…"
}
