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

// Context compaction runs as an ADK BeforeModelCallback, the only v1.4.0 hook
// that sees the assembled request before it hits the model. The ADK runner
// rebuilds every request from the full session history with no trimming, so a
// node's worker session (worker tool loop + the trust gate's self-critique /
// revision rounds, all in one session) grows past the model's window on large
// research runs. The callback fires before each model call and, when the request
// would overflow, summarises the older turns via a separate summariser model
// into an anchored summary kept in session state, then DROPS those turns
// (reusing the stored summary when the live tail still fits, so most turns cost
// no summariser call).
//
// Compaction folds old turns into KNOWLEDGE; it never hollows them out in place.
// The original port of opencode's cheap "prune" pass blanked old tool-output
// payloads while keeping the call (opencode does the same:
// packages/opencode/src/session/message-v2.ts:293 substitutes "[Old tool result
// content cleared]" for a compacted part), leaving the model looking at
//
//	FunctionCall:     read_file("crates/goose/src/agents/mod.rs")
//	FunctionResponse: "[earlier tool output elided to fit the context window]"
//
// — evidence that the work happened with the learning deleted, whose only
// rational response is to redo the call (live code run: the same file read 8x,
// list_dir "." 9x per node). opencode survives it because it is driven by
// huge-context frontier models where prune rarely fires; quack runs a 65k window
// on a local model, so it fired constantly. Worse, prune ran FIRST and could
// satisfy the budget on its own, short-circuiting the summariser, so the
// knowledge was destroyed before anything could preserve it.
//
// The invariant now (goose's and OpenHands' design): after compaction the model
// never sees a tool call whose result was replaced by a placeholder — either the
// turn is gone, its content folded into the anchored summary, or it is intact.
// goose summarises and then hides the old messages
// (crates/goose/src/context_mgmt/mod.rs compact_messages), telling the model its
// context was compacted; OpenHands' LLMSummarizingCondenser
// (sdk/context/condenser/llm_summarizing_condenser.py) drops the forgotten events
// and replaces them with a single summary event.
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
	overheadKey      = "quack.compaction.overhead"       // provider tokens NOT explained by req.Contents (system + tool schemas): ADDITIVE, not a multiplier

	// Calibration ratio bounds. The floor is the DEFAULT (not 1.0) on purpose:
	// undercounting is the fatal failure mode — a provider 400 strands the
	// node's session for good, while over-compaction merely costs context — and
	// the raw bytes/4 estimate structurally CANNOT see the system-instruction +
	// tool-schema overhead the provider bills, an overhead that only grows as a
	// session accretes tools/history. A turn whose measured prompt tokens happen
	// to land at or below the estimate (prose the provider tokenizes more
	// efficiently than 4 chars/token, a whitespace/markup-heavy turn) would, with
	// a 1.0 floor, store calibration 1.0 and then trust it FOREVER — the estimate
	// stops being inflated at all and the very next code-dense revise round
	// undercounts and 400s (live spiral 2026-07-12: implement node, ratio=1,
	// calibrated_tokens≈54k over a 45k budget, judge rounds 4–6 each dead in ~1m).
	// So we never trust a factor below the 1.3 safety margin; a real measurement
	// only ever scales the estimate UP from there. The ceiling caps freak ratios
	// (a tiny request whose measurement is dominated by tool schemas, or a media
	// turn the estimate doesn't count) so a single skewed turn can't force
	// pathological over-compaction forever — the ratio is re-measured every turn.
	minCalibrationRatio = defaultCalibrationRatio
	maxCalibrationRatio = 8.0
	// Before any measurement exists (first model call of a session) we apply a
	// mildly conservative default rather than trusting bytes/4: chars/4 already
	// runs slightly under on prose and badly under on code, and the first
	// request carries the full unseen system-prompt + tool-schema overhead.
	// Triggering compaction a touch early is survivable; overflowing is not.
	// minCalibrationRatio is pinned to this so a degenerate measured≤estimate
	// turn can never drag the trusted ratio below the first-turn safety margin.
	defaultCalibrationRatio = 1.3
)

// Compaction carries the per-agent compaction settings into Build.
type Compaction struct {
	Summarizer    model.LLM // summariser model (its own model, opencode runs a hidden agent)
	ContextWindow int       // the agent model's total context window in tokens (0 ⇒ disabled)
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

// enforceBudget runs the reuse-summary → summarise ladder, mutating req.Contents
// in place. Every "did we free enough?" comparison uses the CALIBRATED estimate,
// not raw bytes/4: the ratio folds in both the true tokenizer density of the
// session's content and the fixed request overhead (system instruction + tool
// declarations) that estimateTokens can't see — there's no need to model the
// overhead separately.
func enforceBudget(ctx adkagent.Context, c Compaction, budget int, req *model.LLMRequest) {
	ratio := calibrationRatio(ctx)
	overhead := intState(ctx, overheadKey)
	if measuredInput(ctx) < budget && calibrated(estimateTokens(req.Contents), ratio, overhead) <= budget {
		return
	}
	// Reuse fast-path: session events are append-only, so the summary's
	// coverage boundary is a stable index. If the anchored summary plus the
	// live tail since it still fits, reapply it with NO summariser call — only
	// re-summarise when that tail has itself grown past budget.
	if prev, n := readSummary(ctx); prev != "" && n > 1 && n <= len(req.Contents) {
		reused := append([]*genai.Content{mergeTaskSummary(req.Contents[0], prev)}, req.Contents[n:]...)
		if calibrated(estimateTokens(reused), ratio, overhead) <= budget {
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
	// Last-resort backstop: summarisation leaves contents[0] verbatim
	// (it's the self-contained task / revise prompt), so if contents[0] ALONE
	// overflows the budget nothing above could have helped and the request is
	// unsendable — the provider 400s and the session is stranded for good (the
	// revise-round spiral, live failure 2026-07-12). Truncate the MIDDLE of an
	// oversized contents[0] with a loud marker: losing the middle of an over-long
	// task beats a dead session, and the worker still has its tools + (for a
	// coding node) the repo on disk. Primary defence is not building a pathological
	// contents[0] in the first place (see vetting.buildRevisionContent bounds);
	// this only fires if some prompt still balloons past the window.
	//
	// This must key on contents[0]'s OWN size, not the whole request's. It used to
	// test the whole request, so anything that inflated the estimate — notably a
	// calibration ratio pinned at its 8.0 ceiling, which happens whenever the
	// provider's FIXED overhead (system instruction + ~20 tool schemas, easily 6k
	// tokens) dwarfs a small contents[0] — made it shred a task that was never too
	// big. Live: a 728-character code-explorer task was truncated to nothing, and the
	// worker woke up asking "Which repository would you like me to explore?".
	if head := calibrated(estimateTokens(req.Contents[:1]), ratio, overhead); head > budget {
		if truncateHeadToFit(req.Contents, budget, ratio) {
			slog.Warn("compaction truncated an oversized task/revise prompt (contents[0]) as a last resort",
				"component", "agent", "budget", budget, "head_tokens", head, "ratio", ratio, "session", ctx.SessionID())
		}
	}
	// Compaction is out of moves; a calibrated size still over budget predicts a
	// hard provider 400 next (which permanently strands the session), so this is
	// worth a Warn even though the request is still sent.
	if got := calibrated(estimateTokens(req.Contents), ratio, overhead); got > budget {
		slog.Warn("compaction could not bring request under budget; context overflow likely",
			"component", "agent", "calibrated_tokens", got, "budget", budget, "ratio", ratio, "session", ctx.SessionID())
	}
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
// The summary carries compactionNotice so the model knows the turns it can no
// longer see are folded into it (goose tells the model the same thing) and does
// not treat the missing history as work still to do.
func mergeTaskSummary(task *genai.Content, summary string) *genai.Content {
	return &genai.Content{
		Role:  task.Role,
		Parts: append(append([]*genai.Part{}, task.Parts...), &genai.Part{Text: compactionNotice + summary}),
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
		// Solve measured ≈ overhead + density×estimate for the OVERHEAD, holding the
		// density at its conservative default. One equation, one unknown — and the
		// overhead is the part that is actually fixed (system + tool schemas), so it
		// generalises to the next turn instead of exploding on small content.
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

// calibrated scales a raw bytes/4 estimate into approximate real prompt tokens.
// calibrated models the provider's prompt tokens as
//
//	measured ≈ overhead + density × estimate(req.Contents)
//
// The overhead (system instruction + ~20 tool schemas — easily 6k tokens) is FIXED
// and cannot be seen by estimateTokens; the density corrects bytes/4 for code-dense
// content. Modelling that overhead as a MULTIPLIER — the old `estimate * ratio` —
// blows up precisely when the content is small: a node's first turn carries a
// 728-char task (~200 tokens estimated) while the provider bills ~6000, so
// measured/estimate ≈ 30, pinned to the 8.0 ceiling. Every later turn was then
// multiplied by 8, and a genuinely ~7k-token request was declared to be 56344 —
// a number ~49k of which never existed. Compaction then panicked, found nothing to
// free on a fresh session, and shredded contents[0]: the worker lost its task and
// woke up asking "Which repository would you like me to explore?".
func calibrated(estimate int, density float64, overhead int) int {
	return overhead + int(float64(estimate)*density)
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
