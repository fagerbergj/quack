package vetting

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

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
}

const verifyBatch = 20

const verifyInstruction = `You check whether a passage of evidence supports a specific detail in a claim. For EVERY numbered item answer one object {"n": <number>, "state": "supported" | "unsupported" | "cannot_tell", "quote": <verbatim text copied from that item's <Evidence>>}. "supported" means the evidence states the same detail (the same figure, date or wording, allowing rounding words like "about"); "unsupported" means the evidence gives a different value or contradicts it; "cannot_tell" means the evidence does not settle it. The quote must be copied exactly from the item's own <Evidence> and, for supported or unsupported, must contain the value you relied on; a quote that is not in the evidence is rejected. Respond with exactly one JSON object {"items": [...]}, nothing else.`

// VerifyChecks runs the verify tier over located checks, batched per cited page,
// and fills each check's Verdict. A failed call leaves its items not_checked.
func (v Verifier) VerifyChecks(ctx context.Context, checks []UnitCheck) []UnitCheck {
	if v.LLM == nil {
		return checks
	}
	byPage := map[string][]int{}
	for i, c := range checks {
		if c.State == "located" || (c.State == "unlocated" && c.Window != "") {
			byPage[c.Citation] = append(byPage[c.Citation], i) // an unlocated figure with a key-term window gets its second look
		}
	}
	pages := make([]string, 0, len(byPage))
	for p := range byPage {
		pages = append(pages, p)
	}
	sort.Strings(pages)
	for _, p := range pages {
		idx := byPage[p]
		for start := 0; start < len(idx); start += verifyBatch {
			v.verifyBatch(ctx, checks, idx[start:min(len(idx), start+verifyBatch)])
		}
	}
	return checks
}

func (v Verifier) verifyBatch(ctx context.Context, checks []UnitCheck, idx []int) {
	var b strings.Builder
	for n, i := range idx {
		c := checks[i]
		fmt.Fprintf(&b, "%d. <Claim>%s</Claim>\n   <Detail>%s</Detail>\n   <Evidence>%s</Evidence>\n\n", n+1, c.Unit.Text, c.Specific.Value, c.Window)
	}
	raw, err := v.ask(ctx, b.String())
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
	if _, ok := locateQuote(c.Window, strings.ToLower(a.Quote)); !ok || strings.TrimSpace(a.Quote) == "" {
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
