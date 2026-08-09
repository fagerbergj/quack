package otelobs

import "testing"

// otlp*http 1.45 stopped appending the default signal path to a path-less
// endpoint, so the deployed http://otel-collector:4318 would post to / and lose
// telemetry silently. These pin the join instead of trusting the exporter.
func TestSignalURL(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint, path, want string
	}{
		{"path-less gets the signal path", "http://otel-collector:4318", "/v1/metrics", "http://otel-collector:4318/v1/metrics"},
		{"trailing slash does not double up", "http://otel-collector:4318/", "/v1/traces", "http://otel-collector:4318/v1/traces"},
		{"an explicit path is left alone", "http://collector:4318/custom", "/v1/metrics", "http://collector:4318/custom"},
		{"https path-less", "https://otel.example.com", "/v1/traces", "https://otel.example.com/v1/traces"},
		{"host:port without scheme", "otel-collector:4318", "/v1/metrics", "otel-collector:4318/v1/metrics"},
		{"no scheme but a path is left alone", "otel-collector:4318/x", "/v1/traces", "otel-collector:4318/x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := signalURL(tc.endpoint, tc.path); got != tc.want {
				t.Errorf("signalURL(%q, %q) = %q, want %q", tc.endpoint, tc.path, got, tc.want)
			}
		})
	}
}
