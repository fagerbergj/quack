package wizard

import (
	"testing"

	"github.com/fagerbergj/quack/internal/cli"
)

// TestModelGroupsMergesAttachmentModels covers onboarding.md audit finding
// 12: vision and audio share one screen instead of two, cutting the
// local-init screen count without losing either question.
func TestModelGroupsMergesAttachmentModels(t *testing.T) {
	groups := modelGroups(&cli.InitAnswers{}, nil, true)
	if len(groups) != 4 {
		t.Errorf("modelGroups returned %d groups, want 4 (main, judge, embed, attachment)", len(groups))
	}
}

// TestStoreGroupsLocalCollapsesToOneConfirm covers onboarding.md audit
// finding 12: the local branch of `quack init` gates the four store screens
// behind one confirm; `quack server init` (the Docker/operator persona)
// keeps all four, since it needs the real backends.
func TestStoreGroupsLocalCollapsesToOneConfirm(t *testing.T) {
	feats := []string{}
	if got := len(storeGroups(&cli.InitAnswers{}, &feats, true)); got != 5 {
		t.Errorf("local storeGroups = %d groups, want 5 (1 confirm + 4 gated)", got)
	}
	if got := len(storeGroups(&cli.InitAnswers{}, &feats, false)); got != 4 {
		t.Errorf("non-local storeGroups = %d groups, want 4 (unchanged)", got)
	}
}
