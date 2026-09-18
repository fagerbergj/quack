package sleeperkinds

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"

	"github.com/fagerbergj/quack/internal/recordstore"
)

// TestKindsRegistered checks every sleeper artifact kind lands in the
// closed set agent-card.json's "artifact" field may select from.
func TestKindsRegistered(t *testing.T) {
	for _, kind := range kindNames {
		if err := recordstore.ValidateArtifactKind(kind); err != nil {
			t.Errorf("kind %q: %v", kind, err)
		}
	}
}

// TestFixturesValidateAgainstSchema checks the copied schemas (source of
// truth: quack-extensions/sleeper/ui/schemas) still accept the copied UI
// fixtures (quack-extensions/sleeper/ui/fixtures) - catches the two drifting
// out of sync with each other.
func TestFixturesValidateAgainstSchema(t *testing.T) {
	for _, kind := range kindNames {
		t.Run(kind, func(t *testing.T) {
			raw, err := os.ReadFile("testdata/fixtures/" + kind + ".json")
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			var instance any
			if err := json.Unmarshal(raw, &instance); err != nil {
				t.Fatalf("parse fixture: %v", err)
			}

			schemaRaw, err := schemaFS.ReadFile("schemas/" + kind + ".json")
			if err != nil {
				t.Fatalf("read schema: %v", err)
			}
			var schema jsonschema.Schema
			if err := json.Unmarshal(schemaRaw, &schema); err != nil {
				t.Fatalf("parse schema: %v", err)
			}
			resolved, err := schema.Resolve(nil)
			if err != nil {
				t.Fatalf("resolve schema: %v", err)
			}
			if err := resolved.Validate(instance); err != nil {
				t.Errorf("fixture fails schema: %v", err)
			}
		})
	}
}
