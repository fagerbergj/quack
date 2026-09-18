// Package sleeperkinds registers the recordstore artifact kinds the Sleeper
// agent bundles' agent-card.json "artifact" field names - the SDK module has no quack import and can't call recordstore.Register itself.
package sleeperkinds

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"

	"github.com/fagerbergj/quack/internal/recordstore"
)

// schemaFS holds copies of quack-extensions/sleeper's ui/schemas/*.json (v0.1.0/83b77b7) - update these if that module's schemas change.
//go:embed schemas/*.json
var schemaFS embed.FS

// Blob-class, not Structured: ArtifactKindNames() (what agent-card.json's
// "artifact" may name) only ever lists Blob kinds.
var kindNames = []string{"lineup", "waivers", "trends", "season-notes"}

func init() {
	for _, kind := range kindNames {
		b, err := schemaFS.ReadFile("schemas/" + kind + ".json")
		if err != nil {
			panic(fmt.Sprintf("sleeperkinds: missing embedded schema for %q: %v", kind, err))
		}
		recordstore.Register(kind, recordstore.KindSpec{
			Class:      recordstore.Blob,
			JSONSchema: string(b),
			Identity:   contentOrHintIdentity,
		})
	}
}

// contentOrHintIdentity: hint if given, else a content hash - mirrors the
// schema-less blob fallback in internal/vetting's kindText/kindBytes.
func contentOrHintIdentity(content []byte, hint string) (string, error) {
	if hint != "" {
		return hint, nil
	}
	h := sha256.Sum256(content)
	return hex.EncodeToString(h[:])[:8], nil
}
