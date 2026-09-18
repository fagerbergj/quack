// Package sleeperkinds registers the recordstore artifact kinds the Sleeper
// extension's agent bundles (agents/lineup-analyst, waiver-scout, trend-scout)
// declare on agent-card.json's "artifact" field. The extension module
// (github.com/fagerbergj/quack-extensions/sleeper) has no quack import and so
// cannot call recordstore.Register itself; quack owns these kinds instead,
// keyed to the same JSON Schema files the extension's own UI renders against
// (copied from quack-extensions/sleeper/ui/schemas at v0.1.0/83b77b7 - update
// schemas/*.json here if that module's schemas change).
package sleeperkinds

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"

	"github.com/fagerbergj/quack/internal/recordstore"
)

//go:embed schemas/*.json
var schemaFS embed.FS

// Kinds are Blob-class (not Structured): write_artifact accepts any
// registered Blob kind by name with no registry-level body validation, and
// ArtifactKindNames() - the set agent-card.json's "artifact" field may name -
// only ever lists Blob kinds. The schema still rides along on JSONSchema for
// documentation and the free well-formedness check recordstore.Register does.
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

// contentOrHintIdentity: hint if the caller gave one (e.g. a job's
// chat-derived subject), else a content hash - mirrors the schema-less blob
// fallback in internal/vetting's kindText/kindBytes.
func contentOrHintIdentity(content []byte, hint string) (string, error) {
	if hint != "" {
		return hint, nil
	}
	h := sha256.Sum256(content)
	return hex.EncodeToString(h[:])[:8], nil
}
