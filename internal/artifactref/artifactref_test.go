package artifactref

import (
	"testing"

	"google.golang.org/genai"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	cases := []struct {
		name              string
		userID, sessionID string
		fileName          string
		revision          int64
		mime              string
	}{
		{"plain", "local", "chat-123", "photo.png", 1, "image/png"},
		{"weird filename", "local", "chat-123", "my photo #1 (final).png", 3, "image/png"},
		{"github user", "octocat", "github-owner-repo-42", "diagram.svg", 7, "image/svg+xml"},
		{"unicode filename", "local", "chat-abc", "写真.jpg", 2, "image/jpeg"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			part := Encode(c.userID, c.sessionID, c.fileName, c.revision, c.mime)
			if part.FileData == nil {
				t.Fatalf("Encode did not produce a FileData part")
			}
			if part.FileData.MIMEType != c.mime {
				t.Errorf("MIMEType = %q, want %q", part.FileData.MIMEType, c.mime)
			}
			uid, sid, name, rev, ok := Decode(part)
			if !ok {
				t.Fatalf("Decode failed on our own Encode output: uri=%q", part.FileData.FileURI)
			}
			if uid != c.userID || sid != c.sessionID || name != c.fileName || rev != c.revision {
				t.Errorf("Decode = (%q,%q,%q,%d), want (%q,%q,%q,%d)",
					uid, sid, name, rev, c.userID, c.sessionID, c.fileName, c.revision)
			}
		})
	}
}

func TestDecodeRejectsNonReferences(t *testing.T) {
	cases := []struct {
		name string
		part *genai.Part
	}{
		{"nil part", nil},
		{"inline data", &genai.Part{InlineData: &genai.Blob{Data: []byte("x"), MIMEType: "text/plain"}}},
		{"text", &genai.Part{Text: "hello"}},
		{"foreign scheme", &genai.Part{FileData: &genai.FileData{FileURI: "gs://bucket/object", MIMEType: "image/png"}}},
		{"empty uri", &genai.Part{FileData: &genai.FileData{MIMEType: "image/png"}}},
		{"malformed path", &genai.Part{FileData: &genai.FileData{FileURI: "quack-artifact:///only/two", MIMEType: "image/png"}}},
		{"missing version", &genai.Part{FileData: &genai.FileData{FileURI: "quack-artifact:///u/s/n", MIMEType: "image/png"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, _, _, _, ok := Decode(c.part); ok {
				t.Errorf("Decode unexpectedly accepted %+v", c.part)
			}
		})
	}
}

// A reference part must never satisfy the InlineData-carrying checks the
// rest of the codebase uses to detect real bytes (e.g. the OpenAI adapter,
// SaveRequest.Validate) - it is a FileData part, nothing else.
func TestEncodeNeverSetsInlineDataOrText(t *testing.T) {
	part := Encode("u", "s", "n", 1, "image/png")
	if part.InlineData != nil {
		t.Errorf("Encode set InlineData; a reference part must never carry bytes")
	}
	if part.Text != "" {
		t.Errorf("Encode set Text; a reference part must be FileData only")
	}
}
