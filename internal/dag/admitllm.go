package dag

import (
	"context"
	"iter"

	"google.golang.org/adk/v2/model"
)

// AdmittingLLM wraps an LLM so a generate call holds admission capacity only
// while it is actually generating. The orchestrator needs this: its run spans
// the whole DAG, but it sits idle while worker nodes execute - holding a
// session across that span deadlocks any config whose sessions cap is at or
// below the number of concurrent runs (the orchestrators would own every
// session their own nodes are waiting for).
type AdmittingLLM struct {
	model.LLM
	admission *Admission
	spec      AdmissionSpec
	onQueued  func()
}

// NewAdmittingLLM returns inner unwrapped when there is nothing to enforce, so
// an unlimited model keeps its original call path. onQueued may be nil.
func NewAdmittingLLM(inner model.LLM, admission *Admission, spec AdmissionSpec, onQueued func()) model.LLM {
	if admission == nil || spec.Model == "" {
		return inner
	}
	if onQueued == nil {
		onQueued = func() {}
	}
	return &AdmittingLLM{LLM: inner, admission: admission, spec: spec, onQueued: onQueued}
}

func (a *AdmittingLLM) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		// Admitted inside the iterator, not at call time: the sequence is lazy,
		// so reserving earlier would hold a session a caller may never consume.
		if !a.admission.Admit(ctx, a.spec, a.onQueued) {
			yield(nil, ctx.Err())
			return
		}
		defer a.admission.Release(a.spec)
		for resp, err := range a.LLM.GenerateContent(ctx, req, stream) {
			if !yield(resp, err) {
				return
			}
		}
	}
}
