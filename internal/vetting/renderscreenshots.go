// renderscreenshots.go: hands render-check's per-story screenshots to the
// judge as `bytes:` artifacts (#1211, follow-up to #1192). Scoping reuses
// the check-command name itself as the "area:frontend" signal - render-check
// only exists in a frontend package.json, so no separate area concept is
// needed.
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
// allowlist ("npm run" prefix) permits for this to apply (#1192).
const renderCheckCommand = "npm run render-check"

// maxJudgeScreenshots caps image evidence handed to the judge per round -
// bounded like every other judge-prompt input, not a full gallery dump.
const maxJudgeScreenshots = 6

// renderCheckScreenshotDir: where render-check.browser.test.tsx saves PNGs,
// relative to the checked package dir (sibling of its own src/ - see that
// file's page.screenshot call).
const renderCheckScreenshotDir = "render-check"

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
