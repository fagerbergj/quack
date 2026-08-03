package vetting

import (
	"context"
	"testing"

	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/fagerbergj/quack/internal/otelobs"
)

type captureEvalExporter struct{ records []sdklog.Record }

func (c *captureEvalExporter) Export(_ context.Context, records []sdklog.Record) error {
	c.records = append(c.records, records...)
	return nil
}
func (c *captureEvalExporter) Shutdown(context.Context) error   { return nil }
func (c *captureEvalExporter) ForceFlush(context.Context) error { return nil }

// TestEmitEvaluationResults_DeterministicOrder guards against map iteration
// (v.Criteria) making the emitted event order flap run to run - a
// replay/diff consumer needs a stable order, not just a stable set.
func TestEmitEvaluationResults_DeterministicOrder(t *testing.T) {
	capExp := &captureEvalExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	v := verdict{Criteria: map[string]criterionScore{
		"zebra_criterion":  {Score: 0.5, Reason: "z"},
		"alpha_criterion":  {Score: 0.9, Reason: "a"},
		"middle_criterion": {Score: 0.7, Reason: "m"},
	}}

	for i := 0; i < 5; i++ {
		capExp.records = nil
		emitEvaluationResults(context.Background(), "judge-r1", v)
		if len(capExp.records) != 3 {
			t.Fatalf("iteration %d: got %d records, want 3", i, len(capExp.records))
		}
		var names []string
		for _, r := range capExp.records {
			r.WalkAttributes(func(kv otellog.KeyValue) bool {
				if kv.Key == otelobs.GenAIEvaluationName {
					names = append(names, kv.Value.AsString())
				}
				return true
			})
		}
		want := []string{"alpha_criterion", "middle_criterion", "zebra_criterion"}
		if len(names) != len(want) {
			t.Fatalf("iteration %d: got %v, want %v", i, names, want)
		}
		for j := range want {
			if names[j] != want[j] {
				t.Errorf("iteration %d: order = %v, want alphabetical %v", i, names, want)
				break
			}
		}
	}
}
