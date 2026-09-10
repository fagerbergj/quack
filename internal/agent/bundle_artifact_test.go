package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fagerbergj/quack/internal/recordstore"
)

// testArtifactKind is a throwaway blob-class kind, registered once so
// LoadBundle's card.Artifact validation has a real registered kind to accept
// - registering in init() (not per-test) since Register panics on a
// duplicate name.
const testArtifactKind = "bundle_test_artifact_kind"

func init() {
	recordstore.Register(testArtifactKind, recordstore.KindSpec{
		Class:    recordstore.Blob,
		Identity: func(_ []byte, hint string) (string, error) { return hint, nil },
	})
}

// TestLoadBundleArtifactField covers the card's optional "artifact" field:
// a registered kind is accepted, an unregistered one is rejected, and
// omitting it entirely (the default) is fine.
func TestLoadBundleArtifactField(t *testing.T) {
	t.Run("valid registered kind", func(t *testing.T) {
		card, err := json.Marshal(map[string]any{"name": "x", "artifact": testArtifactKind})
		if err != nil {
			t.Fatal(err)
		}
		b, err := LoadBundle(writeBundle(t, string(card), "prompt"))
		if err != nil {
			t.Fatalf("LoadBundle: %v", err)
		}
		if b.Card.Artifact != testArtifactKind {
			t.Errorf("Card.Artifact = %q, want %q", b.Card.Artifact, testArtifactKind)
		}
	})

	t.Run("unregistered kind rejected", func(t *testing.T) {
		card := `{"name":"x","artifact":"not-a-real-kind"}`
		_, err := LoadBundle(writeBundle(t, card, "prompt"))
		if err == nil || !strings.Contains(err.Error(), "not-a-real-kind") {
			t.Errorf("err = %v, want a rejection naming the bad kind", err)
		}
	})

	t.Run("omitted defaults empty", func(t *testing.T) {
		b, err := LoadBundle(writeBundle(t, `{"name":"x"}`, "prompt"))
		if err != nil {
			t.Fatalf("LoadBundle: %v", err)
		}
		if b.Card.Artifact != "" {
			t.Errorf("Card.Artifact = %q, want empty when the field is omitted", b.Card.Artifact)
		}
	})
}
