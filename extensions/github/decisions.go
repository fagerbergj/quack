package github

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fagerbergj/quack/extensions/sdk"
)

// decideTimeout bounds each Decide call and each GitHub read the state needs.
// Above the host's 20 s handler timeout so the host decides; Clef serves one request at a time, so asks queue.
var decideTimeout = 25 * time.Second

// Caps keep every state under ~3.5k tokens for a 4096-token handler; caps count runes.
const (
	hunkCap    = 4000
	findingCap = 4000
	prBodyCap  = 2000
	commentCap = 6000
	titleCap   = 300
	filesCap   = 50
	// verdictBudget bounds the whole review.verdict state: findings fill it before files and hunks.
	verdictBudget  = 9500
	findingsBudget = 6000
	filesBudget    = 1200
	minFindingCap  = 120
)

// cutMarker ends a field the verdict budget cut.
const cutMarker = "…[truncated]"

// reviewLabelRe mirrors quack-core's commentLabelRe, the label set quack counts toward a verdict.
var reviewLabelRe = regexp.MustCompile(`(?i)^\s*[^\pL\pN*]{0,4}\*{0,2}(blocking|suggestion|nit|question)\b[^:]*:\*{0,2}`)

// carriedOverRe matches the prefix quack puts on a finding carried over from a prior review.
var carriedOverRe = regexp.MustCompile(`^\(carried over[^)]*\)\s*`)

// severityLabels names the labels and severity markers a finding body can carry, which are baselines, not content.
const severityLabels = `(?:blocking|non-blocking|suggestion|nit|question|must[- ]fix|should[- ]fix|critical|major|minor)`

// inlineLabelRe matches a label anywhere in a finding: line-start before a colon, or in bold or brackets.
var inlineLabelRe = regexp.MustCompile(`(?im)^[ \t]*[^\pL\pN*\n]{0,4}\*{0,2}` + severityLabels + `(?:\s*\([^)\n]*\))?\s*:\*{0,2}[ \t]*` +
	`|\*\*` + severityLabels + `(?:\s*\([^)\n]*\))?:?\*\*:?[ \t]*` + `|\[` + severityLabels + `\][ \t]*`)

var (
	severityQuestions = map[string]sdk.DecisionQuestion{"severity": {
		Type:         "choice",
		Instructions: "Which Conventional Comments label fits this code-review finding, given the diff hunk it is anchored to?",
		Criteria: map[string]string{
			"blocking":   "must be fixed before the pull request merges",
			"suggestion": "a worthwhile improvement that need not block the merge",
			"nit":        "trivial style or polish",
			"question":   "asks for clarification rather than requesting a change",
		},
	}}
	blockingQuestions = map[string]sdk.DecisionQuestion{"blocking": {
		Type:         "noul",
		Instructions: "Would merging with this finding unaddressed be a mistake?",
	}}
	verdictQuestions = map[string]sdk.DecisionQuestion{"verdict": {
		Type:         "choice",
		Instructions: "Which review verdict should this pull request get, given its changes and the review's findings?",
		Criteria: map[string]string{
			"approve":         "ready to merge as it is",
			"comment":         "feedback that neither approves nor blocks",
			"request_changes": "must change before it can merge",
		},
	}}
	intentQuestions = map[string]sdk.DecisionQuestion{
		"write": {Type: "noul",
			Instructions: "Does this message ask quack to change code, push, merge, label, or otherwise write to the repository?"},
		"deliverable": {Type: "choice",
			Instructions: "What should quack produce in answer to this message?",
			Criteria: map[string]string{
				"reply":        "a comment answering the message, with no new work",
				"review":       "a code review of the pull request",
				"commit":       "a commit pushed to this pull request",
				"pull_request": "code written and a new pull request opened",
				"plan":         "an implementation plan posted as a comment",
			}},
	}
	flakyQuestions = map[string]sdk.DecisionQuestion{"flaky": {
		Type:         "noul",
		Instructions: "Is this CI failure unrelated to the PR's changes (a flaky or infrastructure failure)?",
	}}
)

var decisionPoints = []sdk.DecisionPoint{
	{Name: "finding.severity", Description: "the Conventional Comments label of one review finding",
		Questions: severityQuestions, Primary: "severity", Restrictive: []string{"blocking"}, Modes: []string{"observe"}},
	{Name: "finding.blocking", Description: "whether one review finding should block the merge",
		Questions: blockingQuestions, Primary: "blocking", Restrictive: []string{"true"}, Modes: []string{"observe"}},
	{Name: "finding.raise", Description: "whether a non-blocking review finding should be raised to blocking before the review posts",
		Questions: blockingQuestions, Primary: "blocking", Restrictive: []string{"true"}, Modes: []string{"guard"}},
	{Name: "review.verdict", Description: "the verdict of a whole review",
		Questions: verdictQuestions, Primary: "verdict", Restrictive: []string{"comment", "request_changes"}, Modes: []string{"observe"}},
	{Name: "intent", Description: "whether a comment asks quack to write to the repository, and which deliverable it asks for",
		Questions: intentQuestions, Primary: "write", Restrictive: []string{"false"}, Modes: []string{"observe"}},
	{Name: "ci.flaky", Description: "whether one failed check on a PR quack is fixing is unrelated to the PR's changes",
		Questions: flakyQuestions, Primary: "flaky", Modes: []string{"observe"}},
}

// DecisionPoints declares the review, intent and CI points, observe only, and finding.raise, guard
// only: the one point whose answer changes a posted review, and only toward blocking.
func (e *Extension) DecisionPoints() []sdk.DecisionPoint { return decisionPoints }

type intentState struct {
	Subject     string `json:"subject"` // "issue" or "pull request"
	Title       string `json:"title"`
	Sender      string `json:"sender"`
	Association string `json:"author_association,omitempty"`
	Grant       string `json:"grant"` // the delivery kinds the labels and authorship allow
	Comment     string `json:"comment"`
}

type ciFailureState struct {
	Repo         string   `json:"repo"`
	PR           int      `json:"pr"`
	HeadSHA      string   `json:"head_sha"`
	Check        string   `json:"check"`
	Failure      string   `json:"failure"`
	ChangedFiles []string `json:"changed_files"`
}

type findingState struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Finding string `json:"finding"`
	Hunk    string `json:"hunk,omitempty"`
}

type verdictFinding struct {
	Path    string `json:"path"`
	Line    int    `json:"line"`
	Finding string `json:"finding"`
}

// verdictState holds facts only: the reviewer's severities and summary prose stay out.
type verdictState struct {
	Title    string           `json:"pr_title"`
	Body     string           `json:"pr_body,omitempty"`
	Findings []verdictFinding `json:"findings"`
	Files    []string         `json:"changed_files,omitempty"`
	Hunks    []string         `json:"hunks,omitempty"`
}

// splitLabel returns a finding's Conventional-Comments label ("" if none) and its text
// without any label or severity marker, so the baseline never reaches a state.
func splitLabel(body string) (label, text string) {
	text = carriedOverRe.ReplaceAllString(strings.TrimSpace(body), "")
	first, _, _ := strings.Cut(text, "\n")
	if m := reviewLabelRe.FindStringSubmatchIndex(first); m != nil {
		label, text = strings.ToLower(text[m[2]:m[3]]), text[m[1]:]
	}
	return label, strings.TrimSpace(inlineLabelRe.ReplaceAllString(text, ""))
}

// clip cuts s to n runes, marking the cut.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:max(n-utf8.RuneCountInString(cutMarker), 0)]) + cutMarker
}

// fitVerdict bounds st to verdictBudget runes: title and body, then findings, then files, then hunks with what is left.
func fitVerdict(st verdictState, hunks []string) verdictState {
	st.Title, st.Body = clip(st.Title, titleCap), clip(st.Body, prBodyCap)
	room := verdictBudget - utf8.RuneCountInString(st.Title+st.Body)
	if n := len(st.Findings); n > 0 {
		per := max(minFindingCap, min(room, findingsBudget)/n)
		if keep := max(1, min(room, findingsBudget)/per); keep < n {
			st.Findings = append(st.Findings[:keep:keep], verdictFinding{Finding: fmt.Sprintf("%s %d more findings", cutMarker, n-keep)})
		}
		for i := range st.Findings {
			st.Findings[i].Finding = clip(st.Findings[i].Finding, per)
			room -= utf8.RuneCountInString(st.Findings[i].Path+st.Findings[i].Finding) + 35 // JSON keys and line
		}
	}
	st.Files = fitList(st.Files, min(room, filesBudget))
	for _, f := range st.Files {
		room -= utf8.RuneCountInString(f)
	}
	st.Hunks = fitList(hunks, room)
	return st
}

// fitList keeps items within n runes, cutting the one that crosses it and counting any dropped.
func fitList(items []string, n int) []string {
	var out []string
	for i, it := range items {
		if l := utf8.RuneCountInString(it); l <= n {
			out, n = append(out, it), n-l
			continue
		}
		rest := len(items) - i
		if n > 2*len(cutMarker) {
			out, rest = append(out, clip(it, n)), rest-1
		}
		if rest > 0 {
			out = append(out, fmt.Sprintf("%s %d more", cutMarker, rest))
		}
		return out
	}
	return out
}

// hunkAt returns the @@ hunk of patch whose new side covers line, or "".
func hunkAt(patch string, line int) string {
	for i, h := range strings.Split(patch, "\n@@") {
		if i > 0 {
			h = "@@" + h
		}
		if parsePatch(h).right[line] {
			return h
		}
	}
	return ""
}

// observeReview asks the review's shadow questions off the posting path, so a
// slow or failing handler can never delay or fail the review.
func (a *App) observeReview(ctx context.Context, owner, repo string, number int, verdict string, comments []sdk.ReviewComment) {
	if a.decide == nil {
		return
	}
	go a.decideReview(context.WithoutCancel(ctx), owner, repo, number, verdict, comments)
}

// decideReview runs the calls one at a time: a burst per finding can exhaust the handler's GPU batch.
func (a *App) decideReview(ctx context.Context, owner, repo string, number int, verdict string, comments []sdk.ReviewComment) {
	pctx, cancel := context.WithTimeout(ctx, decideTimeout)
	positions, _ := a.commentablePositions(pctx, owner, repo, number)
	cancel()
	seen, seenHunk := map[sdk.ReviewComment]bool{}, map[string]bool{}
	var findings []verdictFinding
	var hunks []string
	for _, c := range comments {
		if seen[c] {
			continue
		}
		seen[c] = true
		label, text, st, hunk := findingOf(positions, c)
		if hunk != "" && !seenHunk[hunk] {
			seenHunk[hunk] = true
			hunks = append(hunks, hunk)
		}
		if label != "" {
			a.ask(ctx, sdk.DecideRequest{Point: "finding.severity", State: st, Baseline: label})
		}
		a.ask(ctx, sdk.DecideRequest{Point: "finding.blocking", State: st, Baseline: strconv.FormatBool(label == "blocking")})
		findings = append(findings, verdictFinding{Path: c.Path, Line: c.Line, Finding: text})
	}
	verdict = strings.ToLower(strings.TrimSpace(verdict))
	if !reviewEvents[strings.ToUpper(verdict)] {
		return
	}
	st := verdictState{Findings: findings, Files: slices.Sorted(maps.Keys(positions))}
	mctx, cancel := context.WithTimeout(ctx, decideTimeout)
	defer cancel()
	if m, err := a.pullMeta(mctx, owner, repo, number); err == nil {
		st.Title, st.Body = strings.TrimSpace(m.Title), strings.TrimSpace(m.Body)
	}
	a.ask(ctx, sdk.DecideRequest{Point: "review.verdict", State: fitVerdict(st, hunks), Baseline: verdict})
}

// findingOf is c's label (the host's Severity first, then the body's), its text without labels, its
// state, and the "path\nhunk" it anchors to ("" when outside the diff).
func findingOf(positions map[string]diffPositions, c sdk.ReviewComment) (label, text string, st findingState, hunk string) {
	label, text = splitLabel(c.Body)
	if sev := strings.ToLower(strings.TrimSpace(c.Severity)); severityQuestions["severity"].Criteria.(map[string]string)[sev] != "" {
		label = sev
	}
	st = findingState{Path: c.Path, Line: c.Line, Finding: truncate(text, findingCap)}
	if p, err := resolvePath(positions, c.Path); err == nil {
		if h := hunkAt(positions[p].patch, c.Line); h != "" {
			st.Hunk, hunk = truncate(h, hunkCap), p+"\n"+h
		}
	}
	return label, text, st, hunk
}

// guardBudget bounds a review's whole wait on finding.raise; findings past it post as labeled.
var guardBudget = 60 * time.Second

// raisedFinding is a finding finding.raise moved to blocking, with the model's probability.
type raisedFinding struct {
	sdk.ReviewComment
	p float64
}

// guardFindings asks finding.raise about each distinct non-blocking finding before the review posts.
// The point is guard-only, so an answer means guard mode; the first error (disabled or down) stops asking.
func (a *App) guardFindings(ctx context.Context, owner, repo string, number int, comments []sdk.ReviewComment) []raisedFinding {
	if a.decide == nil || len(comments) == 0 {
		return nil
	}
	deadline := time.Now().Add(guardBudget)
	var positions map[string]diffPositions
	var raised []raisedFinding
	seen := map[sdk.ReviewComment]bool{}
	for _, c := range comments {
		if seen[c] || time.Now().After(deadline) {
			continue
		}
		seen[c] = true
		if positions == nil {
			pctx, cancel := context.WithTimeout(ctx, decideTimeout)
			positions, _ = a.commentablePositions(pctx, owner, repo, number)
			cancel()
		}
		label, _, st, _ := findingOf(positions, c)
		if label == "blocking" {
			continue
		}
		dctx, cancel := context.WithTimeout(ctx, decideTimeout)
		d, err := a.decide(dctx, sdk.DecideRequest{Point: "finding.raise", State: st, Baseline: "false"})
		cancel()
		if err != nil {
			slog.Debug("github: no finding.raise decision", "component", "github", "err", err)
			return raised
		}
		if d.Restrict {
			raised = append(raised, raisedFinding{c, d.TopP})
		}
	}
	return raised
}

// raiseFindings relabels the raised comments blocking; with any raised, an approve becomes request_changes
// (in event's own case, VERDICT tail too) and the body opens with what moved.
func raiseFindings(event, body string, comments []sdk.ReviewComment, raised []raisedFinding) (string, string, []sdk.ReviewComment) {
	if len(raised) == 0 {
		return event, body, comments
	}
	p := map[sdk.ReviewComment]float64{}
	var where []string
	for _, r := range raised {
		p[r.ReviewComment] = r.p
		where = append(where, fmt.Sprintf("`%s:%d` (p=%.2f)", r.Path, r.Line, r.p))
	}
	out := make([]sdk.ReviewComment, len(comments))
	for i, c := range comments {
		if prob, ok := p[c]; ok {
			c.Severity, c.Body = "blocking", blockingBody(c.Body, prob)
		}
		out[i] = c
	}
	moved := ""
	if strings.EqualFold(event, "approve") {
		event = map[bool]string{true: "REQUEST_CHANGES", false: "request_changes"}[event == "APPROVE"]
		body = verdictRe.ReplaceAllString(body, "VERDICT: request_changes")
		moved = " The verdict moved from approve to request changes."
	}
	banner := fmt.Sprintf("> [!CAUTION]\n> quack's decision model rates %d finding(s) this review left non-blocking as blocking: %s.%s\n\n",
		len(raised), strings.Join(where, ", "), moved)
	return event, banner + body, out
}

// blockingBody swaps a finding's leading label (after any carried-over prefix) for a raised blocking one.
func blockingBody(body string, p float64) string {
	prefix := carriedOverRe.FindString(body)
	rest := strings.TrimPrefix(body, prefix)
	if loc := reviewLabelRe.FindStringIndex(rest); loc != nil {
		rest = rest[loc[1]:]
	}
	return prefix + fmt.Sprintf("**blocking** (raised by quack's decision model, p=%.2f): ", p) + strings.TrimSpace(rest)
}

// observeIntent asks the intent point for a free-text comment trigger, off the
// dispatch path; kind is "" for synthetic and command triggers, which nothing classifies.
func (e *Extension) observeIntent(ctx context.Context, p issueCommentPayload, chatID string, allowedKinds []string, kind string) {
	if e.app.decide == nil || kind == "" || p.isLabelTrigger {
		return
	}
	subject := "issue"
	if p.Issue.PullRequest != nil {
		subject = "pull request"
	}
	st := intentState{Subject: subject, Title: truncate(p.Issue.Title, titleCap), Sender: p.Comment.User.Login,
		Association: p.Comment.AuthorAssociation, Grant: permissionsText(allowedKinds), Comment: truncate(p.Comment.Body, commentCap)}
	write := kind == "commit" || kind == "pull_request"
	go e.app.ask(context.WithoutCancel(ctx), sdk.DecideRequest{Point: "intent", State: st, Baseline: strconv.FormatBool(write), ChatID: chatID})
}

// observeCIFailure asks ci.flaky once per failing check, off the fix path. The
// baseline is empty: a scorer joins (head_sha, check) to the check's later conclusion.
func (e *Extension) observeCIFailure(ctx context.Context, chatID string, ri repoInfo, number int, sha string, checks []failingCheck) {
	if e.app.decide == nil || len(checks) == 0 {
		return
	}
	go e.app.decideCIFailure(context.WithoutCancel(ctx), chatID, ri.Owner, ri.Name, number, sha, checks)
}

func (a *App) decideCIFailure(ctx context.Context, chatID, owner, repo string, number int, sha string, checks []failingCheck) {
	fctx, cancel := context.WithTimeout(ctx, decideTimeout)
	files, _ := a.pullFiles(fctx, owner, repo, number)
	cancel()
	names := []string{}
	for _, f := range files[:min(len(files), filesCap)] {
		names = append(names, f.Filename)
	}
	for _, c := range checks[:min(len(checks), maxFailingChecks)] {
		st := ciFailureState{Repo: owner + "/" + repo, PR: number, HeadSHA: sha, Check: c.Name,
			Failure: truncate(renderOneCheck(c), maxChecksContextRunes), ChangedFiles: names}
		a.ask(ctx, sdk.DecideRequest{Point: "ci.flaky", State: st, ChatID: chatID})
	}
}

func (a *App) ask(ctx context.Context, req sdk.DecideRequest) {
	ctx, cancel := context.WithTimeout(ctx, decideTimeout)
	defer cancel()
	if _, err := a.decide(ctx, req); err != nil {
		slog.Debug("github: no decision", "component", "github", "point", req.Point, "err", err)
	}
}
