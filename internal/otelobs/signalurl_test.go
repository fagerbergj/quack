package otelobs

import "testing"

// otlp*http 1.45 stopped appending the default signal path to a path-less
// endpoint, so the deployed http://otel-collector:4318 would post to / and lose
// telemetry silently. These pin the join instead of trusting the exporter.
//
// The path-bearing cases are #1045: refusing to append to an endpoint that
// already had a path made Langfuse's /api/public/otel base unusable for EVERY
// signal, with no configuration that worked.
func TestSignalURL(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, path, want string
	}{
		{"path-less gets the signal path", "http://otel-collector:4318", "/v1/metrics", "http://otel-collector:4318/v1/metrics"},
		{"trailing slash does not double up", "http://otel-collector:4318/", "/v1/traces", "http://otel-collector:4318/v1/traces"},
		{"a base URL with a path still gets the signal path (#1045)", "http://collector:4318/custom", "/v1/metrics", "http://collector:4318/custom/v1/metrics"},
		{"langfuse's otlp base", "http://langfuse:3008/api/public/otel", "/v1/traces", "http://langfuse:3008/api/public/otel/v1/traces"},
		{"an endpoint already ending in the signal path does not double up", "http://langfuse:3008/api/public/otel/v1/traces", "/v1/traces", "http://langfuse:3008/api/public/otel/v1/traces"},
		{"https path-less", "https://otel.example.com", "/v1/traces", "https://otel.example.com/v1/traces"},
		{"host:port without scheme", "otel-collector:4318", "/v1/metrics", "otel-collector:4318/v1/metrics"},
		{"no scheme with a path still appends", "otel-collector:4318/x", "/v1/traces", "otel-collector:4318/x/v1/traces"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := signalURL(tc.endpoint, tc.path); got != tc.want {
				t.Errorf("signalURL(%q, %q) = %q, want %q", tc.endpoint, tc.path, got, tc.want)
			}
		})
	}
}
