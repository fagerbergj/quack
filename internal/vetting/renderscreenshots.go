// renderscreenshots.go: hands render-check's per-story screenshots to the judge as `bytes:` artifacts.
// Fires only on an explicit `npm run render-check` in the node's `checks:`; deriveChecks never emits it.
package vetting

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/recordstore"
)

// renderCheckCommand: the exact npm script the check_commands allowlist must permit.
const renderCheckCommand = "npm run render-check"

// maxJudgeScreenshots caps image evidence handed to the judge per round -
// bounded like every other judge-prompt input, not a full gallery dump.
const maxJudgeScreenshots = 6

// renderCheckScreenshotDir: where render-check.browser.test.tsx saves PNGs, relative to the
// checked package dir.
const renderCheckScreenshotDir = "render-check"

// frontendScreenshotsCriterion: only a rubric declaring this criterion gets screenshots;
// attaching them elsewhere would cost tokens for nothing.
const frontendScreenshotsCriterion = "frontend_screenshots_reviewed"

// renderScreenshotEvidence returns this round's render-check PNGs as judge image parts, each saved as
// a `bytes:` artifact; skipped entirely unless checksRan.
func renderScreenshotEvidence(ctx context.Context, cfg Config, nodeID string, checksRan bool, act workerActivity) []*genai.Part {
	if !checksRan || cfg.Workspace == nil {
		return nil
	}
	if _, ok := cfg.RubricSpecs[frontendScreenshotsCriterion]; !ok {
		return nil
	}
	dir, ok, err := checksDir(cfg)
	if err != nil || !ok {
		return nil
	}
	checks := cfg.Checks
	if len(checks) == 0 {
		checks = deriveChecks(dir, cfg.CheckCommands)
	}
	if !slices.Contains(checks, renderCheckCommand) {
		return nil
	}
	paths := selectScreenshots(filepath.Join(dir, renderCheckScreenshotDir), act.written)
	if len(paths) == 0 {
		return nil
	}
	c := recordClient(cfg)
	parts := make([]*genai.Part, 0, len(paths))
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if c != nil {
			lineage := recordstore.Lineage{NodeID: nodeID, HeadSHA: cfg.NodeBaseSHA, SavedAt: time.Now().UTC(), Author: "gate"}
			hint := nodeID + ":" + filepath.Base(p)
			if _, _, err := c.SaveBlob(ctx, kindBytes, data, "image/png", hint, lineage); err != nil {
				slog.Warn("render-check screenshot artifact save failed",
					"component", "vetting", "node", nodeID, "file", filepath.Base(p), "err", err)
			}
		}
		parts = append(parts, &genai.Part{InlineData: &genai.Blob{Data: data, MIMEType: "image/png"}})
	}
	return parts
}

// attachScreenshots returns a judge-only copy of question; question.Parts is never mutated,
// so it stays safe to reuse for revision prompts.
func attachScreenshots(question *genai.Content, shots []*genai.Part) *genai.Content {
	if len(shots) == 0 {
		return question
	}
	return &genai.Content{Role: question.Role, Parts: append(append([]*genai.Part{}, question.Parts...), shots...)}
}

// hasInlineData: does question carry a binary part, i.e. is a text-only retry worth it.
func hasInlineData(question *genai.Content) bool {
	for _, p := range question.Parts {
		if p != nil && p.InlineData != nil {
			return true
		}
	}
	return false
}

// stripInlineData: a copy of question without InlineData parts, for the text-only judge retry.
func stripInlineData(question *genai.Content) *genai.Content {
	out := &genai.Content{Role: question.Role, Parts: make([]*genai.Part, 0, len(question.Parts))}
	for _, p := range question.Parts {
		if p != nil && p.InlineData == nil {
			out.Parts = append(out.Parts, p)
		}
	}
	return out
}

// selectScreenshots picks at most maxJudgeScreenshots PNGs deterministically: changed stories first,
// then the rest strided evenly so the sample spans the suite, not its alphabetic head.
func selectScreenshots(dir string, written []string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var all []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".png") {
			all = append(all, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(all)
	if len(all) <= maxJudgeScreenshots {
		return all
	}
	changed, rest := partitionByChangedStory(all, written)
	out := changed
	if len(out) > maxJudgeScreenshots {
		out = out[:maxJudgeScreenshots]
	}
	// ponytail: fixed stride, not a weighted sample; swap in a smarter sampler if coverage gaps show up.
	remaining := maxJudgeScreenshots - len(out)
	if remaining > 0 && len(rest) > 0 {
		stride := len(rest) / remaining
		if stride < 1 {
			stride = 1
		}
		for i := 0; i < len(rest) && len(out) < maxJudgeScreenshots; i += stride {
			out = append(out, rest[i])
		}
	}
	return out
}

// partitionByChangedStory: paths whose name contains a changed file's basename first,
// then the rest, both in sorted order.
func partitionByChangedStory(files, written []string) (changed, rest []string) {
	for _, f := range files {
		if screenshotMatchesChange(filepath.Base(f), written) {
			changed = append(changed, f)
		} else {
			rest = append(rest, f)
		}
	}
	return changed, rest
}

// screenshotMatchesChange: name is "<safeName>__story__viewport__theme.png", where safeName maps the
// module path's non-alnum chars to '_'; the changed basename is mangled the same way.
func screenshotMatchesChange(name string, written []string) bool {
	safeName, _, ok := strings.Cut(name, "__")
	if !ok {
		return false
	}
	for _, w := range written {
		key := sanitizeLikeRenderCheck(filepath.Base(w))
		if key != "" && strings.Contains(safeName, key) {
			return true
		}
	}
	return false
}

// sanitizeLikeRenderCheck mirrors render-check.browser.test.tsx's
// `path.replace(/[^a-zA-Z0-9]/g, '_')`.
func sanitizeLikeRenderCheck(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		default:
			return '_'
		}
	}, s)
}
