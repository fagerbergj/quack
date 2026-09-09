// renderscreenshots.go: hands render-check's per-story screenshots to the
// judge as `bytes:` artifacts . The trigger is an
// explicit `npm run render-check` entry in the node's own `checks:` list -
// deriveChecks never emits it, so this never fires implicitly on frontend
// nodes.
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

// renderCheckCommand: the exact npm script config/quack.yaml's check_commands
// allowlist ("npm run" prefix) permits for this to apply .
const renderCheckCommand = "npm run render-check"

// maxJudgeScreenshots caps image evidence handed to the judge per round -
// bounded like every other judge-prompt input, not a full gallery dump.
const maxJudgeScreenshots = 6

// renderCheckScreenshotDir: where render-check.browser.test.tsx saves PNGs,
// relative to the checked package dir (sibling of its own src/ - see that
// file's page.screenshot call).
const renderCheckScreenshotDir = "render-check"

// frontendScreenshotsCriterion is the rubric criterion name that scores
// attached screenshots (agents/code-reviewer/rubric.yaml). Only nodes whose
// resolved rubric declares it get screenshots - an implementer's rubric has
// no such criterion, so attaching there would only cost tokens for nothing.
const frontendScreenshotsCriterion = "frontend_screenshots_reviewed"

// renderScreenshotEvidence returns this round's render-check PNGs as
// judge-ready image parts, saving each as a `bytes:` artifact scoped to
// nodeID. checksRan gates the whole path (computeDeterministicCriteria
// already knows whether checks executed this round) so a node whose checks
// were skipped never touches the filesystem here. Re-derives the checks
// list itself (cheap: cfg.Checks, or a filesystem stat via deriveChecks)
// rather than threading a new return value through checksPassCriterion's
// dozen existing call sites for one extra bit of information.
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

// attachScreenshots returns a judge-only copy of question with shots
// appended - the worker's own question.Parts slice is never mutated, so the
// same content stays safe to reuse for revision prompts.
func attachScreenshots(question *genai.Content, shots []*genai.Part) *genai.Content {
	if len(shots) == 0 {
		return question
	}
	return &genai.Content{Role: question.Role, Parts: append(append([]*genai.Part{}, question.Parts...), shots...)}
}

// hasInlineData reports whether question carries any image (or other
// binary) part - used to decide whether a judge failure is worth a
// text-only retry.
func hasInlineData(question *genai.Content) bool {
	for _, p := range question.Parts {
		if p != nil && p.InlineData != nil {
			return true
		}
	}
	return false
}

// stripInlineData returns a copy of question with InlineData parts removed,
// for the one-shot degrade-to-text-only judge retry .
func stripInlineData(question *genai.Content) *genai.Content {
	out := &genai.Content{Role: question.Role, Parts: make([]*genai.Part, 0, len(question.Parts))}
	for _, p := range question.Parts {
		if p != nil && p.InlineData == nil {
			out.Parts = append(out.Parts, p)
		}
	}
	return out
}

// selectScreenshots picks at most maxJudgeScreenshots PNGs from dir,
// deterministically: screenshots whose filename references a story/component
// this round actually changed come first, then the rest are strided evenly
// so the sample spans the whole suite rather than just its alphabetic head.
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
	// ponytail: fixed stride, not a weighted sample - good enough to spread
	// coverage across the suite; a smarter sampler can replace this if a
	// real gap in coverage shows up in practice.
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

// partitionByChangedStory splits screenshot paths into ones whose filename
// prefix (render-check's sanitized module path) contains a changed file's
// basename, first, then the rest, both in their original (sorted) order.
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

// screenshotMatchesChange: name is "<safeName>__story__viewport__theme.png";
// safeName mangles the module path's non-alnum chars to '_' (render-check's
// own convention) - match a changed file's basename, mangled the same way.
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
