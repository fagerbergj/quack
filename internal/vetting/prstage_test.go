package vetting

import (
	"strings"
	"testing"
)

// TestAugmentFromPRStage_ToolStagedWins proves a stage_pr-staged PR (the
// implementer authored it via the pr-authoring skill, resolved advisor token →
// MemSecret → MemSession.PRStage) overrides augmentFromRepo's commit-subject
// fallback while keeping the branch the disk probe resolved.
func TestAugmentFromPRStage_ToolStagedWins(t *testing.T) {
	secret, err := NewMemSecret()
	if err != nil {
		t.Fatalf("NewMemSecret: %v", err)
	}
	pr := &PRStage{}
	pr.Set("feat(ui): badge ACP nodes in the header (closes #448)", "## Why\nPer-tool badges were noise.\n\n## What\nOne header pill.")
	RegisterMemSession(secret, MemSession{PRStage: pr})
	defer UnregisterMemSession(secret)

	token := AdvisorThreadToken("plan-1", "node-1")
	RegisterAdvisorThread(token, AdvisorTask{NodeID: "node-1", MemSecret: secret})
	defer UnregisterAdvisorThread(token)

	// The disk probe already staged a fallback PR with the commit-subject body
	// and the real work branch.
	act := workerActivity{stagedDelivery: map[string]StagedDelivery{
		"pr": {Kind: "pull_request", Branch: "quack/work-448", Title: "wip", Body: "Commits:\n- wip"},
	}}
	augmentFromPRStage(&act, token)

	st, ok := act.stagedDelivery["pr"]
	if !ok {
		t.Fatal("pr not staged")
	}
	if !strings.HasPrefix(st.Title, "feat(ui):") || !strings.Contains(st.Body, "## Why") {
		t.Fatalf("tool-staged title/body did not override the fallback: %+v", st)
	}
	if st.Branch != "quack/work-448" {
		t.Fatalf("probed branch lost: %q", st.Branch)
	}
}

// TestAugmentFromPRStage_NoCallKeepsFallback proves that with no stage_pr call
// the commit-subject fallback stands untouched.
func TestAugmentFromPRStage_NoCallKeepsFallback(t *testing.T) {
	secret, err := NewMemSecret()
	if err != nil {
		t.Fatalf("NewMemSecret: %v", err)
	}
	RegisterMemSession(secret, MemSession{PRStage: &PRStage{}}) // minted, never called
	defer UnregisterMemSession(secret)
	token := AdvisorThreadToken("plan-2", "node-2")
	RegisterAdvisorThread(token, AdvisorTask{NodeID: "node-2", MemSecret: secret})
	defer UnregisterAdvisorThread(token)

	act := workerActivity{stagedDelivery: map[string]StagedDelivery{
		"pr": {Kind: "pull_request", Branch: "b", Title: "wip", Body: "Commits:\n- wip"},
	}}
	augmentFromPRStage(&act, token)
	if act.stagedDelivery["pr"].Body != "Commits:\n- wip" {
		t.Fatalf("fallback clobbered without a stage_pr call: %+v", act.stagedDelivery["pr"])
	}
}

// TestPRStage_SetPushTracksOmittedFields pins #724: stage_push's optional
// title/body must reach the gate marked omitted, distinct from an empty
// string the agent explicitly sent - the two must never read the same, since
// downstream delivery blanks the field for the latter but not the former.
func TestPRStage_SetPushTracksOmittedFields(t *testing.T) {
	bare := &PRStage{}
	bare.SetPush("", false, "", false)
	sd, ok := bare.Snapshot()
	if !ok || !sd.TitleOmitted || !sd.BodyOmitted {
		t.Fatalf("bare push: got %+v (ok=%v), want both omitted", sd, ok)
	}

	titleOnly := &PRStage{}
	titleOnly.SetPush("new title", true, "", false)
	sd, ok = titleOnly.Snapshot()
	if !ok || sd.TitleOmitted || !sd.BodyOmitted || sd.Title != "new title" {
		t.Fatalf("title-only push: got %+v (ok=%v)", sd, ok)
	}

	both := &PRStage{}
	both.SetPush("t", true, "b", true)
	sd, ok = both.Snapshot()
	if !ok || sd.TitleOmitted || sd.BodyOmitted || sd.Title != "t" || sd.Body != "b" {
		t.Fatalf("full push: got %+v (ok=%v)", sd, ok)
	}
}

// TestAugmentFromPRStage_PreservesOmittedFlags proves a stage_push snapshot's
// TitleOmitted/BodyOmitted survive the fold into workerActivity - delivery
// reads them off act.stagedDelivery["pr"], not the PRStage directly.
func TestAugmentFromPRStage_PreservesOmittedFlags(t *testing.T) {
	secret, err := NewMemSecret()
	if err != nil {
		t.Fatalf("NewMemSecret: %v", err)
	}
	pr := &PRStage{}
	pr.SetPush("", false, "new body only", true)
	RegisterMemSession(secret, MemSession{PRStage: pr, ExistingPR: true})
	defer UnregisterMemSession(secret)

	token := AdvisorThreadToken("plan-3", "node-3")
	RegisterAdvisorThread(token, AdvisorTask{NodeID: "node-3", MemSecret: secret})
	defer UnregisterAdvisorThread(token)

	act := workerActivity{stagedDelivery: map[string]StagedDelivery{
		"pr": {Kind: "pull_request", Branch: "quack/work-724", Title: "the PR's real title", Body: "the PR's real body"},
	}}
	augmentFromPRStage(&act, token)

	st, ok := act.stagedDelivery["pr"]
	if !ok {
		t.Fatal("pr not staged")
	}
	if !st.TitleOmitted || st.BodyOmitted {
		t.Fatalf("got %+v, want TitleOmitted=true BodyOmitted=false", st)
	}
	if st.Body != "new body only" {
		t.Fatalf("body = %q, want the staged push body", st.Body)
	}
	if st.Branch != "quack/work-724" {
		t.Fatalf("probed branch lost: %q", st.Branch)
	}
}
