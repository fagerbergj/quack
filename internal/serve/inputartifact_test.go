package serve

import (
	"context"
	"testing"

	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/artifactref"
)

// TestWriteExtInputArtifactUnchangedNoNewRevision pins #1010's delta rule:
// re-writing identical bytes for the same name must not mint a new revision.
func TestWriteExtInputArtifactUnchangedNoNewRevision(t *testing.T) {
	st, _, _, artifacts, _ := newExtTestStack(t)
	write := writeExtInputArtifact(st, artifacts)
	read := readExtInputArtifact(st, artifacts)

	chatID := "github-acme-widgets-7"
	rev1, changed1, err := write(chatID, "comments", "application/json", []byte(`[{"id":1}]`))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !changed1 || rev1 != 1 {
		t.Fatalf("first write: rev=%d changed=%v, want rev=1 changed=true", rev1, changed1)
	}

	rev2, changed2, err := write(chatID, "comments", "application/json", []byte(`[{"id":1}]`))
	if err != nil {
		t.Fatalf("re-write same bytes: %v", err)
	}
	if changed2 {
		t.Errorf("re-writing identical bytes reported changed=true")
	}
	if rev2 != rev1 {
		t.Errorf("re-writing identical bytes advanced the revision: %d -> %d", rev1, rev2)
	}

	data, ok := read(chatID, "comments")
	if !ok {
		t.Fatal("read after write: not found")
	}
	if string(data) != `[{"id":1}]` {
		t.Errorf("read returned %q, want the stored bytes", data)
	}
}

// TestWriteExtInputArtifactChangedNewRevision pins the other half: different
// bytes for the same name always mint the next revision.
func TestWriteExtInputArtifactChangedNewRevision(t *testing.T) {
	st, _, _, artifacts, _ := newExtTestStack(t)
	write := writeExtInputArtifact(st, artifacts)

	chatID := "github-acme-widgets-7"
	rev1, _, err := write(chatID, "comments", "application/json", []byte(`[{"id":1}]`))
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	rev2, changed, err := write(chatID, "comments", "application/json", []byte(`[{"id":1},{"id":2}]`))
	if err != nil {
		t.Fatalf("write with new content: %v", err)
	}
	if !changed {
		t.Error("changed content reported changed=false")
	}
	if rev2 != rev1+1 {
		t.Errorf("revision = %d, want %d (rev1+1)", rev2, rev1+1)
	}
}

// TestReadArtifactNeedsBytesPrefix pins the cross-repo contract the docs
// describe (PR #1110 review): the pinned github v0.8.1 manifest renders a
// bare local name ("comments"), but read_artifact (internal/acp/memorymcp.go)
// does an exact FileName match against what WriteArtifact actually stored -
// "bytes:<name>". A worker must prefix the manifest's id before calling
// read_artifact; the bare name alone does not resolve.
func TestReadArtifactNeedsBytesPrefix(t *testing.T) {
	st, _, _, artifacts, _ := newExtTestStack(t)
	write := writeExtInputArtifact(st, artifacts)

	chatID := "github-acme-widgets-7"
	if _, _, err := write(chatID, "comments", "application/json", []byte(`[{"id":1}]`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	userID := st.SessionUserForChat(context.Background(), chatID)

	if _, err := artifacts.Load(context.Background(), &artifact.LoadRequest{
		AppName: artifactref.AppName, UserID: userID, SessionID: chatID, FileName: "comments",
	}); err == nil {
		t.Error(`read_artifact("comments") (the manifest's bare id) resolved - expected not-found`)
	}
	if _, err := artifacts.Load(context.Background(), &artifact.LoadRequest{
		AppName: artifactref.AppName, UserID: userID, SessionID: chatID, FileName: "bytes:comments",
	}); err != nil {
		t.Errorf(`read_artifact("bytes:comments") (the documented workaround) failed: %v`, err)
	}
}

// TestReadExtInputArtifactMissingReturnsNotFound pins the "no baseline" path
// a first dispatch relies on: an artifact never written reads as ok=false,
// not an error.
func TestReadExtInputArtifactMissingReturnsNotFound(t *testing.T) {
	st, _, _, artifacts, _ := newExtTestStack(t)
	read := readExtInputArtifact(st, artifacts)

	data, ok := read("github-acme-widgets-7", "comments")
	if ok || data != nil {
		t.Errorf("read of a never-written artifact = (%v, %v), want (nil, false)", data, ok)
	}
}

// TestSaveExtAttachmentDoesNotCollideWithSameNamedInputArtifact pins #1208's
// review finding: an attachment and a dispatch input artifact both save
// under the "bytes" kind in the same chat session, so a naive save-by-bare-
// name would let an attachment named e.g. "pull" silently overwrite the
// extension's own "bytes:pull" input artifact (or vice versa).
// attachmentHintPrefix ("upload-") is what keeps them apart.
func TestSaveExtAttachmentDoesNotCollideWithSameNamedInputArtifact(t *testing.T) {
	st, _, _, artifacts, _ := newExtTestStack(t)
	write := writeExtInputArtifact(st, artifacts)
	read := readExtInputArtifact(st, artifacts)
	ctx := context.Background()
	chatID := "github-acme-widgets-7"
	userID := st.SessionUserForChat(ctx, chatID)

	// The dispatch writes its own "pull" input artifact first.
	inputBytes := []byte(`{"number":7}`)
	if _, _, err := write(chatID, "pull", "application/json", inputBytes); err != nil {
		t.Fatalf("write input artifact: %v", err)
	}

	// A user attachment named "pull" must land at a different id, not clobber it.
	attachBytes := []byte("not a pull request payload")
	ref, err := saveExtAttachment(ctx, artifacts, userID, chatID, "turn1", "pull", attachBytes, "text/plain")
	if err != nil {
		t.Fatalf("saveExtAttachment: %v", err)
	}
	_, _, attachID, _, ok := artifactref.Decode(ref)
	if !ok {
		t.Fatalf("saveExtAttachment returned an undecodable ref part")
	}
	if attachID == "bytes:pull" {
		t.Fatalf("attachment id collided with the dispatch input artifact's id %q", attachID)
	}

	// The input artifact must still read back untouched.
	data, ok := read(chatID, "pull")
	if !ok {
		t.Fatal("input artifact \"pull\" vanished after the attachment save")
	}
	if string(data) != string(inputBytes) {
		t.Errorf("input artifact \"pull\" content = %q, want unchanged %q", data, inputBytes)
	}
}
