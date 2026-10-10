package dag

import (
	"context"
	"iter"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/ledger"
)

// AdmittingLLM holds admission only while generating. The orchestrator idles across the whole DAG run,
// so holding a session there deadlocks any sessions cap at or below the number of concurrent runs.
type AdmittingLLM struct {
	model.LLM
	admission  *Admission
	spec       AdmissionSpec
	onQueued   func()
	onAdmitted func()
}

// NewAdmittingLLM returns inner unwrapped when there is nothing to enforce. onQueued/onAdmitted may
// be nil: state events for UI persistence.
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

// SetLedgerCoords forwards to the wrapped model so per-round stamps still reach the inference model.
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
			// Release before yielding the complete response: ADK runs tool calls nested in this yield, and the
			// orchestrator's execute() admits a DAG node through this same pool, which would deadlock on our hold.
			if resp != nil && !resp.Partial {
				release()
			}
			if !yield(resp, err) {
				return
			}
		}
	}
}
