package docstore

import "strings"

const (
	// defaultChunkSize / defaultChunkOverlap bound a chunk (in runes) and the
	// overlap windowed between oversized splits — ported from document-pipeline.
	defaultChunkSize    = 1500
	defaultChunkOverlap = 150
)

// chunkMarkdown splits text into chunks no larger than maxSize runes, packing
// whole Markdown sections together and windowing any single section that's too
// big. (Ported from document-pipeline; the section splitter is line-based here to
// avoid a Markdown-parser dependency.)
func chunkMarkdown(text string, maxSize, overlap int) []string {
	var chunks []string
	var cur strings.Builder
	flush := func() {
		if strings.TrimSpace(cur.String()) != "" {
			chunks = append(chunks, strings.TrimRight(cur.String(), "\n"))
		}
		cur.Reset()
	}
	for _, sec := range splitSections(text) {
		switch {
		case runeLen(sec) > maxSize:
			flush()
			chunks = append(chunks, chunkText(sec, maxSize, overlap)...)
		case cur.Len() > 0 && runeLen(cur.String())+runeLen(sec) > maxSize:
			flush()
			cur.WriteString(sec)
		default:
			cur.WriteString(sec)
		}
	}
	flush()
	return chunks
}

// splitSections breaks text at ATX heading lines (`#`..`######` followed by a
// space); each section begins with its heading. Line-based, so it won't treat a
// `#` inside a fenced code block specially — acceptable for transcribed/cleaned
// documents.
func splitSections(text string) []string {
	var sections []string
	var cur strings.Builder
	flush := func() {
		if strings.TrimSpace(cur.String()) != "" {
			sections = append(sections, cur.String())
		}
		cur.Reset()
	}
	for _, line := range strings.Split(text, "\n") {
		if isHeading(line) && cur.Len() > 0 {
			flush()
		}
		cur.WriteString(line)
		cur.WriteString("\n")
	}
	flush()
	return sections
}

func isHeading(line string) bool {
	n := 0
	for n < len(line) && line[n] == '#' {
		n++
	}
	return n >= 1 && n <= 6 && n < len(line) && line[n] == ' '
}

// chunkText splits text into overlapping rune windows; the fallback for a section
// larger than size. Blank fragments are dropped (stores reject blank content).
func chunkText(text string, size, overlap int) []string {
	keep := func(c string) bool { return strings.TrimSpace(c) != "" }
	runes := []rune(text)
	if len(runes) <= size {
		if !keep(text) {
			return nil
		}
		return []string{text}
	}
	step := size - overlap
	if step <= 0 {
		step = size
	}
	var chunks []string
	for i := 0; i < len(runes); i += step {
		end := i + size
		if end > len(runes) {
			end = len(runes)
		}
		if c := string(runes[i:end]); keep(c) {
			chunks = append(chunks, c)
		}
		if end == len(runes) {
			break
		}
	}
	return chunks
}

func runeLen(s string) int { return len([]rune(s)) }
