package dag

import (
	"context"
	"iter"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/ledger"
)

// AdmittingLLM wraps an LLM so a generate call holds admission capacity only
// while it is actually generating. The orchestrator needs this: its run spans
// the whole DAG, but it sits idle while worker nodes execute - holding a
// session across that span deadlocks any config whose sessions cap is at or
// below the number of concurrent runs (the orchestrators would own every
// session their own nodes are waiting for).
type AdmittingLLM struct {
	model.LLM
	admission  *Admission
	spec       AdmissionSpec
	onQueued   func()
	onAdmitted func()
}

// NewAdmittingLLM returns inner unwrapped when there is nothing to enforce, so an unlimited model keeps its call path.
// onQueued/onAdmitted may be nil: state events for UI persistence (#1484).
func NewAdmittingLLM(inner model.LLM, admission *Admission, spec AdmissionSpec, onQueued, onAdmitted func()) model.LLM {
	if admission == nil || spec.Model == "" {
		return inner
	}
	if onQueued == nil {
		onQueued = func() {}
	}
	if onAdmitted == nil {
		onAdmitted = func() {}
	}
	return &AdmittingLLM{LLM: inner, admission: admission, spec: spec, onQueued: onQueued, onAdmitted: onAdmitted}
}

// SetLedgerCoords forwards to the wrapped model so per-round stamps still
// reach the inference model behind this hold (#1482 wraps the worker base).
func (a *AdmittingLLM) SetLedgerCoords(c ledger.Coords) {
	if cs, ok := a.LLM.(interface{ SetLedgerCoords(ledger.Coords) }); ok {
		cs.SetLedgerCoords(c)
	}
}

func (a *AdmittingLLM) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		// Admitted inside the iterator, not at call time: the sequence is lazy,
		// so reserving earlier would hold a session a caller may never consume.
		queued := false
		if !a.admission.Admit(ctx, a.spec, func() { queued = true; a.onQueued() }) {
			yield(nil, ctx.Err())
			return
		}
		if queued {
			a.onAdmitted()
		}
		released := false
		release := func() {
			if !released {
				released = true
				a.admission.Release(a.spec)
			}
		}
		defer release() // fallback: an error, cancellation, or early consumer exit before a complete response
		for resp, err := range a.LLM.GenerateContent(ctx, req, stream) {
			// Release BEFORE yielding the complete response, not after the whole
			// iterator returns: the caller (ADK's own flow) runs tool calls
			// synchronously in reaction to this yield, nested inside this same
			// iteration - for the orchestrator, that includes execute() running
			// a DAG node through this exact admission pool (RunPlanStep). Holding
			// the reservation across that nested call deadlocks the node it just
			// queued: the turn's own reservation never frees until the node it's
			// waiting on is admitted, which can't happen while the turn holds it.
			if resp != nil && !resp.Partial {
				release()
			}
			if !yield(resp, err) {
				return
			}
		}
	}
}
