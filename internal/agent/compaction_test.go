package agent

import (
	"strings"
	"testing"
)

func TestChunkByCharsStaysUnderMax(t *testing.T) {
	// No newlines: forces the safeCut path. Must still bound every chunk.
	s := strings.Repeat("abcd", 50000) // 200000 bytes
	chunks := chunkByChars(s, maxSummarizeInputChars)
	for i, c := range chunks {
		if len(c) > maxSummarizeInputChars {
			t.Errorf("chunk %d is %d bytes, exceeds max %d", i, len(c), maxSummarizeInputChars)
		}
	}
	if got := strings.Join(chunks, ""); got != s {
		t.Errorf("chunks don't reassemble to original (%d vs %d bytes)", len(got), len(s))
	}
}

func TestChunkByCharsPrefersNewlinesAndIsRuneSafe(t *testing.T) {
	// Multibyte runes near the boundary: safeCut must not split one.
	line := strings.Repeat("é", 1000) + "\n" // 'é' is 2 bytes
	s := strings.Repeat(line, 100)           // ~200KB with newlines
	chunks := chunkByChars(s, maxSummarizeInputChars)
	for i, c := range chunks {
		if len(c) > maxSummarizeInputChars {
			t.Errorf("chunk %d is %d bytes, exceeds max", i, len(c))
		}
		if !utf8Valid(c) {
			t.Errorf("chunk %d split a UTF-8 rune", i)
		}
	}
	if got := strings.Join(chunks, ""); got != s {
		t.Errorf("chunks don't reassemble to original")
	}
}

func TestChunkByCharsSmallInputSingleChunk(t *testing.T) {
	s := "short text"
	chunks := chunkByChars(s, maxSummarizeInputChars)
	if len(chunks) != 1 || chunks[0] != s {
		t.Errorf("small input should be one chunk, got %v", chunks)
	}
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "�") == s }
