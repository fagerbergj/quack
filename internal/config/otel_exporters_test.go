package config

import "testing"

// #1045: exporters replace the single otlp_endpoint so traces can go to a trace
// backend while metrics go to a collector - the shape that made Langfuse
// unusable, since it ingests traces only.
func TestOtelExporters_Validation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      []OtelExporter
		wantErr string
	}{
		{"traces to one place, metrics to another", []OtelExporter{
			{Endpoint: "http://otel-collector:4318", Signals: []OtelSignal{SignalMetrics, SignalLogs}},
			{Endpoint: "http://langfuse:3008/api/public/otel", Signals: []OtelSignal{SignalTraces}},
		}, ""},
		{"no exporters is valid (telemetry off)", nil, ""},
		{"an unset endpoint env var drops the exporter, it does not fail startup",
			[]OtelExporter{{Signals: []OtelSignal{SignalTraces}}}, ""},
		{"no signals is a config error, not a silent no-op", []OtelExporter{
			{Endpoint: "http://x:4318"},
		}, "lists no signals"},
		{"unknown signal names the mistake", []OtelExporter{
			{Endpoint: "http://x:4318", Signals: []OtelSignal{"trace"}},
		}, `unknown signal "trace"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := OtelConfig{Exporters: tc.in}
			err := o.applyDefaults()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("want an error containing %q, got none", tc.wantErr)
			case tc.wantErr != "" && !contains(err.Error(), tc.wantErr):
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}

// The shipped config lists one exporter whose endpoint is ${QUACK_OTEL_OTLP_ENDPOINT};
// with the var unset that must mean "export nothing", not "refuse to boot".
func TestOtelExporters_UnsetEndpointIsDropped(t *testing.T) {
	o := OtelConfig{Exporters: []OtelExporter{
		{Endpoint: "", Signals: []OtelSignal{SignalTraces, SignalMetrics}},
		{Endpoint: "http://collector:4318", Signals: []OtelSignal{SignalMetrics}},
	}}
	if err := o.applyDefaults(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(o.Exporters) != 1 || o.Exporters[0].Endpoint != "http://collector:4318" {
		t.Fatalf("exporters = %+v, want only the configured one", o.Exporters)
	}
}

func TestOtelExporter_Wants(t *testing.T) {
	e := OtelExporter{Endpoint: "http://x", Signals: []OtelSignal{SignalTraces, SignalLogs}}
	if !e.Wants(SignalTraces) || !e.Wants(SignalLogs) {
		t.Error("Wants must report declared signals")
	}
	if e.Wants(SignalMetrics) {
		t.Error("Wants must not report an undeclared signal - metrics would go to a trace-only backend")
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})())
}
