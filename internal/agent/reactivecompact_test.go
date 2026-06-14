package agent

import (
	"errors"
	"testing"
)

func TestIsContextOverflowErr(t *testing.T) {
	overflow := []string{
		// The exact llama.cpp / llama-swap shape observed in production.
		"error, status code: 400, status: 400 Bad Request, message: request (67521 tokens) exceeds the available context size (65536 tokens), try increasing it",
		"the input exceeds the available context size",
		"context length exceeded",
		"This model's maximum context length is 8192 tokens",
	}
	for _, m := range overflow {
		if !isContextOverflowErr(errors.New(m)) {
			t.Errorf("should be detected as overflow: %q", m)
		}
	}
	notOverflow := []string{
		"error, status code: 500, internal server error",
		"connection refused",
		"401 unauthorized",
		"",
	}
	for _, m := range notOverflow {
		if isContextOverflowErr(errors.New(m)) {
			t.Errorf("should NOT be overflow: %q", m)
		}
	}
	if isContextOverflowErr(nil) {
		t.Errorf("nil error must not be overflow")
	}
}
