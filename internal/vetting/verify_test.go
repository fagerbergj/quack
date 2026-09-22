package vetting

import (
	"context"
	"iter"
	"testing"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// textLLM answers every call with one fixed text; the verify tier has no tools to call.
type textLLM struct {
	text  string
	calls *int
}

func (textLLM) Name() string { return "text-llm" }

func (m textLLM) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	if m.calls != nil {
		*m.calls++
	}
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: m.text}}}}, nil)
	}
}

func locatedCheck(unit, value, kind, norm, window, citation string) UnitCheck {
	return UnitCheck{Unit: Unit{Text: unit}, Specific: Specific{Kind: kind, Value: value, Norm: norm}, Citation: citation, State: "located", Window: window}
}

func TestVerifyChecks_QuoteMustBeRealAndCarryTheFigure(t *testing.T) {
	checks := []UnitCheck{
		locatedCheck("Users rose 30%.", "30%", "percent", "30", "users rose 30% in the last year", "https://p/1"),
		locatedCheck("Users rose 30%.", "30%", "percent", "30", "users rose 30% in the last year", "https://p/2"),
		locatedCheck("Revenue was 5M.", "5M", "number", "5", "revenue reached 5m last quarter", "https://p/3"),
		{Unit: Unit{Text: "Uncited 9%."}, Specific: Specific{Kind: "percent", Value: "9%", Norm: "9"}, State: "uncited"},
	}
	// p/1: honest quote; p/2: quote not in the evidence; p/3: supported but the quote omits the figure.
	answers := map[string]string{
		"https://p/1": `{"items":[{"n":1,"state":"supported","quote":"users rose 30% in the last year"}]}`,
		"https://p/2": `{"items":[{"n":1,"state":"supported","quote":"users doubled overnight"}]}`,
		"https://p/3": `{"items":[{"n":1,"state":"supported","quote":"revenue reached"}]}`,
	}
	calls := 0
	for cit, text := range answers {
		v := Verifier{LLM: textLLM{text: text, calls: &calls}}
		var sub []UnitCheck
		for _, c := range checks {
			if c.Citation == cit {
				sub = append(sub, c)
			}
		}
		got := v.VerifyChecks(context.Background(), sub)
		switch cit {
		case "https://p/1":
			if got[0].Verdict.State != "supported" {
				t.Errorf("p/1: %+v, want supported", got[0].Verdict)
			}
		case "https://p/2":
			if got[0].Verdict.State != "not_checked" {
				t.Errorf("p/2: %+v, want not_checked (quote is not in the evidence)", got[0].Verdict)
			}
		case "https://p/3":
			if got[0].Verdict.State != "not_checked" {
				t.Errorf("p/3: %+v, want not_checked (quote lacks the figure)", got[0].Verdict)
			}
		}
	}
	if calls != 3 {
		t.Errorf("model calls = %d, want one per page", calls)
	}
	v := Verifier{LLM: textLLM{text: "{}"}}
	if got := v.VerifyChecks(context.Background(), checks[3:]); got[0].Verdict.State != "" {
		t.Errorf("an uncited check must not reach the model: %+v", got[0].Verdict)
	}
}

func TestVerifyChecks_UnparseableAnswerIsNotChecked(t *testing.T) {
	v := Verifier{LLM: textLLM{text: "I cannot say."}}
	got := v.VerifyChecks(context.Background(), []UnitCheck{locatedCheck("a 3 b", "3", "number", "3", "a 3 b", "https://p")})
	if got[0].Verdict.State != "not_checked" {
		t.Fatalf("verdict = %+v, want not_checked, never a negative verdict from a failed verifier", got[0].Verdict)
	}
}
