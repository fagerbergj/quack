package openaimodel

import (
	"bytes"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"

	"google.golang.org/genai"
)

func requirePdftoppm(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("pdftoppm"); err != nil {
		t.Skip("SKIPPING PDF conversion test: pdftoppm (poppler-utils) not on PATH")
	}
}

func fixturePDF(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/fixture-2page.pdf")
	if err != nil {
		t.Fatalf("reading fixture PDF: %v", err)
	}
	return data
}

// TestPDFToImageParts_ConvertsPages verifies a PDF part expands into one image
// content part per page, each carrying a data: URL with the PNG MIME type.
func TestPDFToImageParts_ConvertsPages(t *testing.T) {
	requirePdftoppm(t)

	parts, err := pdfToImageParts(fixturePDF(t))
	if err != nil {
		t.Fatalf("pdfToImageParts: %v", err)
	}
	if len(parts) != 2 {
		t.Fatalf("got %d parts, want 2 (fixture is a 2-page PDF)", len(parts))
	}
	for i, p := range parts {
		if p.OfImageURL == nil {
			t.Fatalf("part %d: not an image content part: %+v", i, p)
		}
		url := p.OfImageURL.ImageURL.URL
		if !strings.HasPrefix(url, "data:image/png;base64,") {
			t.Errorf("part %d URL = %q, want a data:image/png;base64,... URL", i, url)
		}
	}
}

// TestPDFToImageParts_PageCapWarns confirms the page cap is enforced and a Warn
// names how many pages were dropped - no silent truncation.
func TestPDFToImageParts_PageCapWarns(t *testing.T) {
	requirePdftoppm(t)

	old := pdfMaxPages
	pdfMaxPages = 1
	defer func() { pdfMaxPages = old }()

	var logBuf bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	defer slog.SetDefault(oldLogger)

	parts, err := pdfToImageParts(fixturePDF(t))
	if err != nil {
		t.Fatalf("pdfToImageParts: %v", err)
	}
	if len(parts) != 1 {
		t.Fatalf("got %d parts, want 1 (page cap)", len(parts))
	}

	logged := logBuf.String()
	if !strings.Contains(logged, "page cap") {
		t.Errorf("expected a Warn naming the page cap, got log output: %q", logged)
	}
	if !strings.Contains(logged, "dropped_pages=1") {
		t.Errorf("expected the Warn to name the dropped page count, got: %q", logged)
	}
}

// TestPDFToImageParts_MissingBinary needs no pdftoppm installed - it forces the
// absent-binary path via an empty PATH, asserting the error names the missing
// dependency rather than silently dropping the document.
func TestPDFToImageParts_MissingBinary(t *testing.T) {
	t.Setenv("PATH", "")

	_, err := pdfToImageParts(fixturePDF(t))
	if err == nil {
		t.Fatal("pdfToImageParts: got nil error, want one naming the missing pdftoppm binary")
	}
	if !strings.Contains(err.Error(), "pdftoppm") || !strings.Contains(err.Error(), "poppler-utils") {
		t.Errorf("error = %q, want it to name pdftoppm/poppler-utils", err.Error())
	}
}

// TestConvertPDFPart_EndToEnd runs a genai part carrying application/pdf
// through the full per-message conversion (the code path GenerateContent
// actually uses), confirming the PDF-rejection case now expands into image
// parts on the outgoing OpenAI message instead of erroring.
func TestConvertPDFPart_EndToEnd(t *testing.T) {
	requirePdftoppm(t)

	content := &genai.Content{
		Role: "user",
		Parts: []*genai.Part{
			{Text: "what does this document say?"},
			{InlineData: &genai.Blob{MIMEType: "application/pdf", Data: fixturePDF(t)}},
		},
	}
	msgs, err := toOpenAIChatCompletionMessage(content)
	if err != nil {
		t.Fatalf("toOpenAIChatCompletionMessage: %v", err)
	}
	if len(msgs) != 1 || msgs[0].OfUser == nil {
		t.Fatalf("msgs = %+v, want one user message", msgs)
	}
	arr := msgs[0].OfUser.Content.OfArrayOfContentParts
	var imageParts int
	for _, p := range arr {
		if p.OfImageURL != nil {
			imageParts++
		}
	}
	if imageParts != 2 {
		t.Errorf("got %d image parts, want 2 (fixture is a 2-page PDF)", imageParts)
	}
}
