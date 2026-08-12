package openaimodel

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"

	openai "github.com/openai/openai-go/v3"
)

// pdfMaxPages bounds how many leading pages of a PDF attachment get rendered
// (document-ingest inputs can run long); var so tests can shrink it without a
// giant fixture. pdfRenderDPI: ~150 is enough for OCR/handwriting - 300 balloons
// image size for no legibility gain on a scanned page.
// Not safe for concurrent modification - pdfToImageParts calls are sequential today.
var pdfMaxPages = 20

const pdfRenderDPI = 150
const pdfConvertTimeout = 60 * time.Second

var pdfInfoPagesRe = regexp.MustCompile(`(?m)^Pages:\s*(\d+)`)

// pdfToImageParts renders a PDF's pages to PNG (via poppler's pdftoppm) and
// returns one image content part per page, so vision models can consume a
// document directly instead of the "unsupported PDF MIME type" rejection
// (#829). Degrades LOUDLY: a missing pdftoppm or a failed render returns an
// error naming the cause - a document is never silently dropped.
func pdfToImageParts(data []byte) ([]openai.ChatCompletionContentPartUnionParam, error) {
	if _, err := exec.LookPath("pdftoppm"); err != nil {
		return nil, fmt.Errorf("openaimodel: pdftoppm (poppler-utils) is required to read PDF attachments and was not found on PATH: %w", err)
	}

	dir, err := os.MkdirTemp("", "quack-pdf-*")
	if err != nil {
		return nil, fmt.Errorf("openaimodel: creating temp dir for PDF conversion: %w", err)
	}
	defer os.RemoveAll(dir)

	pdfPath := filepath.Join(dir, "in.pdf")
	if err := os.WriteFile(pdfPath, data, 0o600); err != nil {
		return nil, fmt.Errorf("openaimodel: writing PDF attachment to disk: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), pdfConvertTimeout)
	defer cancel()

	warnIfPDFTruncated(ctx, pdfPath)

	outPrefix := filepath.Join(dir, "page")
	cmd := exec.CommandContext(ctx, "pdftoppm", "-png", "-r", strconv.Itoa(pdfRenderDPI),
		"-l", strconv.Itoa(pdfMaxPages), pdfPath, outPrefix)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("openaimodel: pdftoppm failed converting PDF attachment: %w: %s", err, stderr.String())
	}

	pages, err := filepath.Glob(outPrefix + "-*.png")
	if err != nil {
		return nil, fmt.Errorf("openaimodel: listing rendered PDF pages: %w", err)
	}
	if len(pages) == 0 {
		return nil, fmt.Errorf("openaimodel: pdftoppm produced no pages for PDF attachment")
	}
	// pdftoppm's zero-padding width depends on the page range rendered, not
	// document order - sort by the numeric suffix, not lexically.
	sort.Slice(pages, func(i, j int) bool { return pdfPageNum(pages[i]) < pdfPageNum(pages[j]) })

	parts := make([]openai.ChatCompletionContentPartUnionParam, 0, len(pages))
	for _, p := range pages {
		png, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("openaimodel: reading rendered PDF page %s: %w", filepath.Base(p), err)
		}
		b64 := base64.StdEncoding.EncodeToString(png)
		parts = append(parts, openai.ImageContentPart(
			openai.ChatCompletionContentPartImageImageURLParam{
				URL:    fmt.Sprintf("data:image/png;base64,%s", b64),
				Detail: "auto",
			},
		))
	}
	return parts, nil
}

var pdfPageNumRe = regexp.MustCompile(`-(\d+)\.png$`)

func pdfPageNum(path string) int {
	m := pdfPageNumRe.FindStringSubmatch(path)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// warnIfPDFTruncated logs how many trailing pages the pdfMaxPages cap drops -
// no silent caps. Best-effort: if pdfinfo can't report a page count, the
// render below still proceeds capped, just without a dropped-page count.
func warnIfPDFTruncated(ctx context.Context, pdfPath string) {
	out, err := exec.CommandContext(ctx, "pdfinfo", pdfPath).Output()
	if err != nil {
		return
	}
	m := pdfInfoPagesRe.FindSubmatch(out)
	if m == nil {
		return
	}
	total, err := strconv.Atoi(string(m[1]))
	if err != nil || total <= pdfMaxPages {
		return
	}
	slog.Warn("PDF attachment exceeds the page cap; trailing pages dropped",
		"component", "inference", "total_pages", total, "rendered_pages", pdfMaxPages, "dropped_pages", total-pdfMaxPages)
}
