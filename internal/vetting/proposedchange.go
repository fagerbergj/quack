package vetting

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// ProposedChange is the replacement code a finding proposes. It renders as a plain fenced block, never
// GitHub's ```suggestion, whose exact drop-in contract nothing here validates.
type ProposedChange struct {
	Language string `json:"language" jsonschema:"short language tag for the fenced block, e.g. go or yaml"`
	Code     string `json:"code" jsonschema:"the replacement code itself, without fences"`
}

// proposedLanguagePattern is also declared in the finding and stage_review_comment schemas.
const proposedLanguagePattern = `^[A-Za-z0-9][A-Za-z0-9_+#.-]{0,19}$`

var proposedLanguageRe = regexp.MustCompile(proposedLanguagePattern)

// DeclareProposedChangeCaps declares Check's language and non-empty code rules on a proposed_change schema.
func DeclareProposedChangeCaps(s *jsonschema.Schema) {
	s.Properties["language"].Pattern = proposedLanguagePattern
	s.Properties["code"].MinLength = jsonschema.Ptr(1)
}

// Check rejects empty code or a language that is not a short token; a nil change is valid.
func (p *ProposedChange) Check() error {
	if p == nil {
		return nil
	}
	var errs []error
	if strings.TrimSpace(p.Code) == "" {
		errs = append(errs, errors.New("proposed_change.code is empty: pass the replacement code, or omit proposed_change for a question or observation"))
	}
	lang := strings.TrimSpace(p.Language)
	switch {
	case strings.EqualFold(lang, "suggestion"):
		errs = append(errs, errors.New("proposed_change.language can't be \"suggestion\": name the code's language (e.g. go); quack renders a plain fenced block"))
	case !proposedLanguageRe.MatchString(lang):
		errs = append(errs, fmt.Errorf("proposed_change.language must be a short language tag such as go or yaml, got %q", p.Language))
	}
	return errors.Join(errs...)
}

// Fence renders p as a fenced block to append to a finding body, or "" when p is nil.
func (p *ProposedChange) Fence() string {
	if p == nil {
		return ""
	}
	code := strings.Trim(p.Code, "\n")
	fence := "```"
	for strings.Contains(code, fence) {
		fence += "`" // code that itself holds a fence needs a longer one
	}
	return "\n\n" + fence + strings.TrimSpace(p.Language) + "\n" + code + "\n" + fence
}
