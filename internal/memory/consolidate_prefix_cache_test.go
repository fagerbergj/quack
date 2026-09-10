// consolidate_prefix_cache_test.go: measures the per-cluster dedupe prompt's
// shared byte-prefix across two different bursts (mirrors #1324's approach).
package memory

import "testing"

// TestDedupePromptSharedPrefixAcrossClusters: two different bursts must
// share the fixed header as a prefix, or vLLM's cache dies at byte 0.
func TestDedupePromptSharedPrefixAcrossClusters(t *testing.T) {
	clusterA := []neighbour{
		{ID: "11111111-1111-1111-1111-111111111111", Content: "the build command is make build"},
		{ID: "22222222-2222-2222-2222-222222222222", Content: "run make build to build the project"},
	}
	clusterB := []neighbour{
		{ID: "33333333-3333-3333-3333-333333333333", Content: "the frontend uses vite"},
		{ID: "44444444-4444-4444-4444-444444444444", Content: "tests run via make test"},
		{ID: "55555555-5555-5555-5555-555555555555", Content: "the backend is written in go"},
	}

	pA := buildDedupePrompt(clusterA)
	pB := buildDedupePrompt(clusterB)

	const header = "MEMORIES MINTED CLOSE TOGETHER BY THE SAME RUN (dedupe near-identical claims):\n"
	shared := commonPrefixLen(pA, pB)
	t.Logf("dedupe prompt len A=%d B=%d; shared prefix=%d bytes (header=%d)", len(pA), len(pB), shared, len(header))
	if shared < len(header) {
		t.Fatalf("dedupe prompt shared prefix = %d bytes, want >= %d (the fixed header) - "+
			"a per-cluster field has moved ahead of the stable instructions, killing the prefix cache",
			shared, len(header))
	}
}

func commonPrefixLen(a, b string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}
