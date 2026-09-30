// judgeartifacttools.go: judge's own list_artifacts/read_artifact over the chat's
// recordstore.Client, narrowed per round to this node's lineage by judgeView.
package vetting

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/recordstore"
)

// judgeArtifactReadCap bounds one read_artifact reply so a large stored
// artifact can't blow the judge's own prompt budget.
const judgeArtifactReadCap = 24_000

// judgeSeededReadCap bounds one prior read seeded into a retry's prompt (the deliverable keeps judgeArtifactReadCap).
const judgeSeededReadCap = 8_000

// judgeView is one judge round's view of the chat: a foreign node's artifacts are hidden
// unless this node wrote, fetched or read them, or its own lineage wrote a revision.
// hidePages: the verify tier already read the cited pages, so the judge is not shown them.
type judgeView struct {
	foreign   map[string]bool
	own       map[string]bool
	hidePages bool
	mu        sync.Mutex
	history   map[string]bool // per id: some revision was written outside the foreign nodes
}

func newJudgeView(cfg Config, act workerActivity) *judgeView {
	v := &judgeView{foreign: map[string]bool{}, own: act.ownArtifactIDs(), hidePages: cfg.judgePagesChecked, history: map[string]bool{}}
	for _, n := range cfg.ForeignNodes {
		v.foreign[n] = true
	}
	return v
}

type judgeViewKey struct{}

func withJudgeView(ctx context.Context, s *judgeView) context.Context {
	return context.WithValue(ctx, judgeViewKey{}, s)
}

// judgeViewFrom is nil outside a judge round: a direct tool call sees everything.
func judgeViewFrom(ctx context.Context) *judgeView {
	s, _ := ctx.Value(judgeViewKey{}).(*judgeView)
	return s
}

// hidesPage: a web_page the verify tier has already read for this round.
func (s *judgeView) hidesPage(id string) bool {
	return s != nil && s.hidePages && recordstore.KindOf(id) == webPageKind
}

func (s *judgeView) visible(ctx context.Context, c *recordstore.Client, id, author string) bool {
	if s.hidesPage(id) {
		return false
	}
	if s == nil || s.own[id] || !s.foreign[author] {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if seen, ok := s.history[id]; ok {
		return seen
	}
	seen, err := s.writtenInScope(ctx, c, id)
	if err == nil { // a failed lookup hides it for this call only
		s.history[id] = seen
	}
	return seen
}

// writtenInScope: an upstream (or own) revision of id exists, e.g. a fan-in's input a sibling edited last.
func (s *judgeView) writtenInScope(ctx context.Context, c *recordstore.Client, id string) (bool, error) {
	versions, err := c.Versions(ctx, id)
	if err != nil {
		return false, err
	}
	for _, v := range versions {
		_, lin, ok, err := c.LoadVersionWithMeta(ctx, id, v)
		if err != nil {
			return false, err
		}
		if ok && !s.foreign[lin.NodeID] {
			return true, nil
		}
	}
	return false, nil
}

// judgeHiddenKinds: gate-owned records excluded from list_artifacts - a judge
// reading its own prior verdict/delivery decisions as "evidence" is circular.
var judgeHiddenKinds = map[string]bool{kindJudgeRound: true, kindDeliveryRecord: true}

// NewJudgeArtifactTools builds list_artifacts + read_artifact over c (no
// edit/write). Duplicated from internal/tools: tools imports vetting, so the reverse import would cycle.
func NewJudgeArtifactTools(c *recordstore.Client) ([]tool.Tool, error) {
	list, err := newJudgeListArtifactsTool(c)
	if err != nil {
		return nil, fmt.Errorf("vetting: judge list_artifacts: %w", err)
	}
	read, err := newJudgeReadArtifactTool(c)
	if err != nil {
		return nil, fmt.Errorf("vetting: judge read_artifact: %w", err)
	}
	return []tool.Tool{list, read}, nil
}

type judgeListArtifactsArgs struct {
	Kind string `json:"kind,omitempty"`
}

func newJudgeListArtifactsTool(c *recordstore.Client) (tool.Tool, error) {
	return functiontool.New[judgeListArtifactsArgs, string](
		functiontool.Config{
			Name:        "list_artifacts",
			Description: "List this chat's artifacts (id, kind, latest revision, authoring node), optionally filtered by kind.",
		},
		func(ctx agent.Context, a judgeListArtifactsArgs) (string, error) {
			items, err := c.List(ctx, a.Kind)
			if err != nil {
				return "", fmt.Errorf("list_artifacts: %w", err)
			}
			view := judgeViewFrom(ctx)
			var b strings.Builder
			for _, it := range items {
				if judgeHiddenKinds[it.Kind] || !view.visible(ctx, c, it.ID, it.NodeID) {
					continue
				}
				fmt.Fprintf(&b, "%s\trevision=%d\tkind=%s\tnode=%s\n", it.ID, it.Revision, it.Kind, it.NodeID)
			}
			if b.Len() == 0 {
				return "(no artifacts)", nil
			}
			return b.String(), nil
		},
	)
}

// judgeReadArtifactArgs mirrors tools.readArtifactArgs's id/revision/window
// shape (internal/tools/artifacts.go) - the judge needs the same window a
// large artifact requires, not just the worker.
type judgeReadArtifactArgs struct {
	ID       string `json:"id"`
	Revision int    `json:"revision,omitempty"`
	Offset   int    `json:"offset,omitempty"`
	Lines    int    `json:"lines,omitempty"`
}

func newJudgeReadArtifactTool(c *recordstore.Client) (tool.Tool, error) {
	return functiontool.New[judgeReadArtifactArgs, string](
		functiontool.Config{
			Name: "read_artifact",
			Description: "Read an artifact by id (from list_artifacts). Text content is returned inline; binary " +
				"content is base64-encoded. Omit revision for the latest, or pass one to read exactly what the " +
				"worker wrote/edited at that point. Pass offset (a 1-based line number) and/or lines to window a " +
				"large text artifact instead of the whole thing.",
		},
		func(ctx agent.Context, a judgeReadArtifactArgs) (string, error) {
			view := judgeViewFrom(ctx)
			if view.hidesPage(a.ID) {
				return "", errPageChecked(a.ID)
			}
			data, mime, author, ok, err := loadJudgeArtifact(ctx, c, a)
			if err != nil {
				return "", fmt.Errorf("read_artifact: %w", err)
			}
			if !ok || !view.visible(ctx, c, a.ID, author) {
				return "", fmt.Errorf("read_artifact: %s: not found", a.ID)
			}
			return shapeJudgeReadArtifact(data, mime, a, judgeArtifactReadCap), nil
		},
	)
}

func errPageChecked(id string) error {
	return fmt.Errorf("read_artifact: %s: fetched pages are not available to you this round - code already checked every cited specific against them, and any it could not confirm is in CITED EVIDENCE", id)
}

// loadJudgeArtifact loads the latest or a named revision, with its authoring node.
func loadJudgeArtifact(ctx context.Context, c *recordstore.Client, a judgeReadArtifactArgs) (data []byte, mime, author string, ok bool, err error) {
	var lineage recordstore.Lineage
	if a.Revision > 0 {
		data, lineage, ok, err = c.LoadVersionWithMeta(ctx, a.ID, a.Revision)
	} else {
		data, mime, lineage, _, ok, err = c.LatestWithMeta(ctx, a.ID)
	}
	return data, mime, lineage.NodeID, ok, err
}

// judgeArtifactWindowLines caps a windowed read_artifact reply, mirroring
// tools.maxWindowLines.
const judgeArtifactWindowLines = 500

// shapeJudgeReadArtifact: windowed or bounded text, or base64 for binary data
// (mirrors tools.shapeReadArtifact - a judge round hits the same large/binary
// artifacts a worker's own read_artifact call does).
func shapeJudgeReadArtifact(data []byte, mime string, a judgeReadArtifactArgs, limit int) string {
	isText := (mime != "" && (strings.HasPrefix(mime, "text/") || mime == "application/json")) ||
		(mime == "" && utf8.Valid(data))
	if !isText {
		if len(data) > artifactref.InlineMaxBytes {
			return fmt.Sprintf("size: %d bytes (exceeds %d byte read_artifact limit)", len(data), artifactref.InlineMaxBytes)
		}
		return "mime: " + mime + "\n\n" + base64.StdEncoding.EncodeToString(data)
	}
	if a.Offset > 0 || a.Lines > 0 {
		lines := strings.Split(string(data), "\n")
		return judgeWindowLines(lines, a.Offset, a.Lines, len(lines), limit)
	}
	return boundExcerpt(string(data), limit)
}

// judgeWindowLines returns lines[start-1:end] (1-based, capped at total, at
// judgeArtifactWindowLines and at limit chars) plus a navigation footer.
func judgeWindowLines(lines []string, offset, want, total, limit int) string {
	start := offset
	if start < 1 {
		start = 1
	}
	if start > total {
		return fmt.Sprintf("[offset %d is past the end of this artifact (%d lines).]", start, total)
	}
	if want <= 0 || want > judgeArtifactWindowLines {
		want = judgeArtifactWindowLines
	}
	last := min(start+want-1, total)
	end, size := start, len(lines[start-1])
	for end < last && size+1+len(lines[end]) <= limit {
		size += 1 + len(lines[end])
		end++
	}
	body := boundExcerpt(strings.Join(lines[start-1:end], "\n"), limit) // one over-long line still fits
	if end < total {
		return fmt.Sprintf("%s\n\n[lines %d-%d of %d. offset=%d to read further.]", body, start, end, total, end+1)
	}
	return fmt.Sprintf("%s\n\n[lines %d-%d of %d (end).]", body, start, end, total)
}

// JudgeHidesArtifact: the judge round in ctx is not shown id (a web_page the verify tier read).
func JudgeHidesArtifact(ctx context.Context, id string) bool {
	return judgeViewFrom(ctx).hidesPage(id)
}

// BoundJudgeArtifactRead shapes artifact id's body for a judge exactly as the live read_artifact
// tool does, refusing a page the round's verify tier already read.
func BoundJudgeArtifactRead(ctx context.Context, id string, data []byte, mime string, offset, lines int) (string, error) {
	if JudgeHidesArtifact(ctx, id) {
		return "", errPageChecked(id)
	}
	return shapeJudgeReadArtifact(data, mime, judgeReadArtifactArgs{Offset: offset, Lines: lines}, judgeArtifactReadCap), nil
}
