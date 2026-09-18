// Package sleeperkinds registers the Sleeper agent bundles' artifact kinds so
// agent-card.json may name them and the extension UI reads the same names.
package sleeperkinds

import (
	"errors"

	"github.com/fagerbergj/quack/internal/recordstore"
)

// Blob-class, not Structured: ArtifactKindNames() (what agent-card.json's
// "artifact" may name) only ever lists Blob kinds.
var kindNames = []string{"lineup", "waivers", "trends", "season-notes"}

func init() {
	for _, kind := range kindNames {
		recordstore.Register(kind, recordstore.KindSpec{
			Class:        recordstore.Blob,
			Identity:     identityFromHint,
			RequiresHint: true,
		})
	}
}

// identityFromHint mirrors internal/vetting's requireHint (document/pr_body)
// so vetting.deliveryTarget's lookup id and a hinted SaveBlob's write id agree.
func identityFromHint(_ []byte, hint string) (string, error) {
	if hint == "" {
		return "", errors.New("sleeperkinds: hint required for this kind's identity")
	}
	return hint, nil
}
