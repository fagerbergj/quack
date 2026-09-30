package vetting

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// Verdict is the verify tier's answer for one specific: the model reads the
// located window as data and must quote it; code checks the quote is real.
type Verdict struct {
	State  string // "supported", "unsupported", "cannot_tell", "not_checked"
	Quote  string
	Reason string
}

// Verifier is a tool-less one-shot model call over a bounded evidence window.
type Verifier struct {
	LLM       model.LLM
	MaxTokens int32
	// Memo holds final verdicts across one node's rounds, keyed by verifyKey, so an
	// unchanged claim is not read again; nil disables it. Not safe for concurrent use.
	Memo map[string]Verdict
	// TryAdmit takes one more model session for a parallel batch (Config.TryAdmitVerify).
	TryAdmit func() (release func(), ok bool)
	// ThinkingLevel: gates.judge.thinking_level, since the verifier runs on the judge's model;
	// "" sends none. Unset, reasoning can spend the whole MaxTokens before any verdict.
	ThinkingLevel string
}

// verifyKey: the claim, its detail, the page, the exact evidence window and whether that window came
// from a search snippet - a snippet verdict (never a contradiction) must not stand in for the fetched page.
func verifyKey(c UnitCheck) string {
	sum := sha256.Sum256([]byte(withoutLinks(c.Unit.Text) + "\x00" + c.Window))
	return fmt.Sprintf("%s\x00%s:%s\x00%t\x00%x", c.Citation, c.Specific.Kind, c.Specific.Norm, c.snippet, sum)
}

const verifyBatch = 20

// verifyConcurrency bounds parallel verifier calls: the held judge session plus extra
// sessions Admission grants without waiting, so parallelism only uses free capacity.
const verifyConcurrency = 4

const verifyInstruction = `You check whether a passage of evidence supports a specific detail in a claim. The message gives a page's numbered Windows of text, then numbered items; each item's <Evidence> names the one Window that is its evidence. For EVERY numbered item answer one object {"n": <number>, "state": "supported" | "unsupported" | "cannot_tell", "quote": <verbatim text copied from that item's Window>}. "supported" means the evidence states the same detail (the same figure, date or wording, allowing rounding words like "about"); "unsupported" means the evidence gives a different value or contradicts it; "cannot_tell" means the evidence does not settle it. The quote must be copied exactly from the item's own Window and, for supported or unsupported, must contain the value you relied on; a quote that is not in that Window is rejected. Respond with exactly one JSON object {"items": [...]}, nothing else.`

// VerifyChecks runs the verify tier over every check that has a window (located, or
// unlocated with a second-look window), batched per cited page; a failed call leaves its items not_checked.
func (v Verifier) VerifyChecks(ctx context.Context, checks []UnitCheck) []UnitCheck {
	if v.LLM == nil {
		return checks
	}
	var idx []int
	keys := map[int]string{}
	for i, c := range checks {
		if c.State != "located" && (c.State != "unlocated" || c.Window == "") {
			continue // an unlocated figure with a key-term window still gets its second look
		}
		k := verifyKey(c)
		if vd, ok := v.Memo[k]; ok {
			checks[i].Verdict = vd
			continue
		}
		keys[i] = k
		idx = append(idx, i)
	}
	v.verifyByPage(ctx, checks, idx)
	failed := v.recheckUnsupported(ctx, checks, idx)
	for _, i := range idx {
		if v.Memo != nil && checks[i].Verdict.State != "not_checked" && !failed[i] {
			v.Memo[keys[i]] = checks[i].Verdict
		}
	}
	return checks
}

// recheckUnsupported reads every unsupported item of among once more in a window three
// times wider: a false fail costs a revise, so an item stays unsupported only when both reads agree.
// failed: items whose second look got no answer, whose cannot_tell is not worth remembering.
func (v Verifier) recheckUnsupported(ctx context.Context, checks []UnitCheck, among []int) (failed map[int]bool) {
	failed = map[int]bool{}
	first := map[int]Verdict{}
	var idx []int
	for _, i := range among {
		c := checks[i]
		if c.Verdict.State == "unsupported" && c.snippet {
			// A snippet is a few hundred characters of a search index's copy, often stale on a live page.
			checks[i].Verdict = Verdict{State: "cannot_tell", Quote: c.Verdict.Quote, Reason: "the evidence is a search snippet, which can back a specific but not contradict it"}
			continue
		}
		if c.Verdict.State == "unsupported" {
			first[i] = c.Verdict
			checks[i].Window = c.wideWindow()
			idx = append(idx, i)
		}
	}
	v.verifyByPage(ctx, checks, idx)
	for _, i := range idx {
		switch checks[i].Verdict.State {
		case "unsupported":
			checks[i].Verdict.Reason = "confirmed by a second look in a wider window"
		case "supported":
		default:
			failed[i] = checks[i].Verdict.State == "not_checked"
			checks[i].Verdict = Verdict{State: "cannot_tell", Quote: first[i].Quote, Reason: "a second look in a wider window did not confirm it"}
		}
	}
	return failed
}

// verifyByPage asks about idx's items in batches that share one cited page.
func (v Verifier) verifyByPage(ctx context.Context, checks []UnitCheck, idx []int) {
	byPage := map[string][]int{}
	for _, i := range idx {
		byPage[checks[i].Citation] = append(byPage[checks[i].Citation], i)
	}
	pages := make([]string, 0, len(byPage))
	for p := range byPage {
		pages = append(pages, p)
	}
	sort.Strings(pages)
	var batches []verifyJob
	for _, p := range pages {
		idx, windows := byPage[p], pageWindows(checks, p)
		for start := 0; start < len(idx); start += verifyBatch {
			batches = append(batches, verifyJob{idx: idx[start:min(len(idx), start+verifyBatch)], page: p, windows: windows})
		}
	}
	workers, release := v.workers(len(batches))
	defer release()
	jobs := make(chan verifyJob)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() { // each batch writes only its own checks[i]
			for job := range jobs {
				v.verifyBatch(ctx, checks, job)
			}
		})
	}
	for _, b := range batches {
		jobs <- b
	}
	close(jobs)
	wg.Wait()
}

// verifyJob is one batch of items on page, with every evidence window read on that page.
type verifyJob struct {
	idx     []int
	page    string
	windows []string
}

// pageWindows: the distinct evidence windows of every check on page (not only those being asked
// about), sorted, so the same page gives the same prompt prefix across batches and rounds.
func pageWindows(checks []UnitCheck, page string) []string {
	var out []string
	for _, c := range checks {
		if c.Citation == page && c.Window != "" && !slices.Contains(out, c.Window) {
			out = append(out, c.Window)
		}
	}
	sort.Strings(out)
	return out
}

// verifyPrompt: the page and its windows lead and the items naming them trail, so vLLM's
// prefix cache carries the windows across every batch and round over that page.
func verifyPrompt(checks []UnitCheck, job verifyJob) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Page: %s\n\n", job.page)
	for n, w := range job.windows {
		fmt.Fprintf(&b, "<Window %d>\n%s\n</Window %d>\n\n", n+1, w, n+1)
	}
	b.WriteString("Items:\n")
	for n, i := range job.idx {
		c := checks[i]
		fmt.Fprintf(&b, "%d. <Claim>%s</Claim>\n   <Detail>%s</Detail>\n   <Evidence>Window %d</Evidence>\n\n", n+1, c.Unit.Text, c.Specific.Value, slices.Index(job.windows, c.Window)+1)
	}
	return b.String()
}

// workers: the held judge session plus each extra one Admission grants now, up to
// verifyConcurrency; with no ledger (TryAdmit nil) the bound alone applies.
func (v Verifier) workers(batches int) (int, func()) {
	want := min(batches, verifyConcurrency)
	if v.TryAdmit == nil {
		return max(want, 1), func() {}
	}
	n, releases := 1, []func(){}
	for ; n < want; n++ {
		rel, ok := v.TryAdmit()
		if !ok {
			break
		}
		releases = append(releases, rel)
	}
	return n, func() {
		for _, r := range releases {
			r()
		}
	}
}

func (v Verifier) verifyBatch(ctx context.Context, checks []UnitCheck, job verifyJob) {
	idx := job.idx
	raw, err := v.ask(ctx, verifyPrompt(checks, job))
	answers := parseVerifyAnswers(raw)
	for n, i := range idx {
		a, ok := answers[n+1]
		switch {
		case err != nil:
			checks[i].Verdict = Verdict{State: "not_checked", Reason: err.Error()}
		case !ok:
			checks[i].Verdict = Verdict{State: "not_checked", Reason: "no answer for this item"}
		default:
			checks[i].Verdict = checkedVerdict(a, checks[i])
		}
	}
}

type verifyAnswer struct {
	N     int    `json:"n"`
	State string `json:"state"`
	Quote string `json:"quote"`
}

// checkedVerdict keeps a model verdict only when its quote is really in the
// window and, for a figure, contains the figure; otherwise the item is not_checked.
func checkedVerdict(a verifyAnswer, c UnitCheck) Verdict {
	state := strings.ToLower(strings.TrimSpace(a.State))
	if state != "supported" && state != "unsupported" && state != "cannot_tell" {
		return Verdict{State: "not_checked", Reason: "unknown state " + a.State}
	}
	if state == "cannot_tell" {
		return Verdict{State: state, Quote: a.Quote}
	}
	if _, ok := locateQuote(c.Window, strings.ToLower(a.Quote), locateWindow); !ok || strings.TrimSpace(a.Quote) == "" {
		return Verdict{State: "not_checked", Reason: "quote is not in the evidence", Quote: a.Quote}
	}
	if k := c.Specific.Kind; (k == "number" || k == "percent" || k == "currency") && state == "supported" {
		if _, ok := LocateSpecific(a.Quote, c.Specific); !ok {
			return Verdict{State: "not_checked", Reason: "supporting quote does not contain the figure", Quote: a.Quote}
		}
	}
	return Verdict{State: state, Quote: a.Quote}
}

var jsonObjectRe = regexp.MustCompile(`(?s)\{.*\}`)

func parseVerifyAnswers(raw string) map[int]verifyAnswer {
	out := map[int]verifyAnswer{}
	m := jsonObjectRe.FindString(raw)
	if m == "" {
		return out
	}
	var doc struct {
		Items []verifyAnswer `json:"items"`
	}
	if json.Unmarshal([]byte(m), &doc) != nil {
		return out
	}
	for _, it := range doc.Items {
		out[it.N] = it
	}
	return out
}

func (v Verifier) ask(ctx context.Context, prompt string) (string, error) {
	maxTokens := v.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}
	req := &model.LLMRequest{
		Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: prompt}}}},
		Config: &genai.GenerateContentConfig{
			SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: verifyInstruction}}},
			MaxOutputTokens:   maxTokens,
			ThinkingConfig:    judgeThinkingConfig(v.ThinkingLevel),
		},
	}
	var out strings.Builder
	for resp, err := range v.LLM.GenerateContent(ctx, req, false) {
		if err != nil {
			return "", fmt.Errorf("verify: %w", err)
		}
		if resp.Content == nil {
			continue
		}
		for _, p := range resp.Content.Parts {
			if p != nil && !p.Thought && p.Text != "" {
				out.WriteString(p.Text)
			}
		}
	}
	if out.Len() == 0 {
		return "", fmt.Errorf("verify: empty answer")
	}
	return out.String(), nil
}
