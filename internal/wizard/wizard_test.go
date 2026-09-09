package wizard

import (
	"testing"

	"github.com/fagerbergj/quack/internal/cli"
)

func TestModelGroupsMergesAttachmentModels(t *testing.T) {
	groups := modelGroups(&cli.InitAnswers{}, nil, true)
	if len(groups) != 4 {
		t.Errorf("modelGroups returned %d groups, want 4 (main, judge, embed, attachment)", len(groups))
	}
}

// `quack server init` (not local) keeps all four screens - it needs the real backends.
func TestStoreGroupsLocalCollapsesToOneConfirm(t *testing.T) {
	feats := []string{}
	if got := len(storeGroups(&cli.InitAnswers{}, &feats, true)); got != 5 {
		t.Errorf("local storeGroups = %d groups, want 5 (1 confirm + 4 gated)", got)
	}
	if got := len(storeGroups(&cli.InitAnswers{}, &feats, false)); got != 4 {
		t.Errorf("non-local storeGroups = %d groups, want 4 (unchanged)", got)
	}
}
