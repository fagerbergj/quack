package github

import (
	"context"
	"strings"
	"testing"
)

// A large-thread, large-payload dispatch must build an envelope an order of magnitude smaller than
// inlining everything; this asserts a bound, not an exact size.
func TestBuildEnvelopeSizeShrinksOrderOfMagnitude(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)

	// ~50 long-ish seeded comments, none new/edited/deleted since the last dispatch (steady state).
	comments := make([]snapshotComment, 0, 50)
	for i := 0; i < 50; i++ {
		comments = append(comments, snapshotComment{
			ID: int64(i + 1), User: "alice", CreatedAt: "2026-01-01T00:00:00Z",
			Body: strings.Repeat("this comment discusses the plan in detail. ", 20),
		})
	}
	snap := Snapshot{Title: "Long-running issue", Body: strings.Repeat("issue body text. ", 50), Comments: comments}
	delta := Delta{} // nothing new/edited/deleted - the steady-state re-dispatch

	var issue issueCommentPayload
	issue.Issue.Number = 1006
	issue.Action = "created"
	// A large raw webhook payload (~10k of repo metadata) that must not be inlined.
	issue.rawEvent = []byte(`{"action":"created","repository":{` + strings.Repeat(`"field":"noise",`, 400) + `"last":"x"}}`)
	issue.eventName = "issues.labeled"

	gh := githubContext{snap: snap, delta: &delta}
	env := ext.buildEnvelope(context.Background(), issue, "task", gh, nil, nil)

	inputSize := len(snap.Body)
	for _, c := range comments {
		inputSize += len(c.Body)
	}
	inputSize += len(issue.rawEvent)

	t.Logf("input=%d env=%d", inputSize, len(env))
	if len(env) >= inputSize/5 {
		t.Errorf("envelope size %d did not shrink by an order of magnitude vs. the %d bytes of underlying evidence", len(env), inputSize)
	}
	if strings.Contains(env, "gravatar_id") || strings.Contains(env, `"field":"noise"`) {
		t.Errorf("envelope still inlines raw webhook payload noise:\n%s", truncateForLog(env))
	}
}

// Even with every input artifact listed, the manifest stays one compact line per artifact,
// not a second copy of what it points at.
func TestBuildEnvelopeSizeWithManifest(t *testing.T) {
	ext, _ := newTestExtension(t, "http://unused", nil)

	var issue issueCommentPayload
	issue.Issue.Number = 1006
	issue.Action = "created"
	issue.eventName = "issues.labeled"

	manifest := []artifactEntry{
		{Name: "comments", Revision: 4, Changed: true, Note: "47 total, 3 new"},
		{Name: "event", Revision: 4, Changed: false, Note: "issues.labeled"},
		{Name: "timeline", Revision: 1, Changed: false, Note: "12 entries"},
		{Name: "check-runs", Revision: 2, Changed: true, Note: "5 checks, 1 failed"},
		{Name: "annotations-go-test", Revision: 1, Changed: true, Note: "14 annotations"},
	}
	env := ext.buildEnvelope(context.Background(), issue, "task", seedGC(Snapshot{}, 0), nil, manifest)

	want := "<artifacts>\n" +
		`  <artifact id="bytes:comments" revision="4" status="new">47 total, 3 new</artifact>` + "\n" +
		`  <artifact id="bytes:event" revision="4" status="unchanged">issues.labeled</artifact>` + "\n" +
		`  <artifact id="bytes:timeline" revision="1" status="unchanged">12 entries</artifact>` + "\n" +
		`  <artifact id="bytes:check-runs" revision="2" status="new">5 checks, 1 failed</artifact>` + "\n" +
		`  <artifact id="bytes:annotations-go-test" revision="1" status="new">14 annotations</artifact>` + "\n" +
		"</artifacts>\n"
	if !strings.Contains(env, want) {
		t.Errorf("manifest block =\n%s\nwant it to contain:\n%s", truncateForLog(env), want)
	}
	// The manifest is a pointer, not a payload: five entries render in well
	// under 500 chars regardless of how large the artifacts they name are.
	if len(want) > 500 {
		t.Errorf("manifest block is %d chars for 5 entries - no longer a compact pointer", len(want))
	}
}
