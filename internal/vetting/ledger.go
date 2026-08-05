// Workspace activity ledger: fs/git/run_command ops from session events for claim checking.
package vetting

import (
	"fmt"
	"strings"
)

// wsOpSpec: which call args/results summarize a workspace tool. web_fetch (args only) signals web-sourced claims.
type wsOpSpec struct {
	args    []string
	results []string
}

// wsOpSpecs: workspace tools whose outcomes an answer could claim.
var wsOpSpecs = map[string]wsOpSpec{
	"read_file":   {args: []string{"path"}},
	"write_file":  {args: []string{"path"}, results: []string{"bytes", "created"}},
	"edit_file":   {args: []string{"path"}, results: []string{"replacements"}},
	"delete_path": {args: []string{"path"}, results: []string{"deleted"}},
	"run_command": {args: []string{"dir", "command"}, results: []string{"exit_code"}},
	"web_fetch":   {args: []string{"url"}}, // args only; presence signals web-sourced claims

	"git_clone":                 {args: []string{"url", "dir"}, results: []string{"dir", "head", "default_branch"}},
	"git_checkout":              {args: []string{"dir", "ref"}, results: []string{"branch", "head"}},
	"git_status":                {args: []string{"dir"}, results: []string{"branch", "clean"}},
	"git_diff":                  {args: []string{"dir", "ref", "path"}},
	"git_log":                   {args: []string{"dir"}},
	"git_commit":                {args: []string{"dir", "message"}, results: []string{"sha", "files_changed"}},
	"git_branch":                {args: []string{"dir", "name"}, results: []string{"current"}},
	"git_push":                  {args: []string{"dir"}, results: []string{"remote", "branch", "sha"}},
	"git_pull":                  {args: []string{"dir"}, results: []string{"branch", "sha", "updated"}},
	"git_rebase":                {args: []string{"dir", "onto"}, results: []string{"sha", "rebased"}},
	"git_worktree_create":       {args: []string{"dir", "branch"}, results: []string{"path"}},
	"git_worktree_remove":       {args: []string{"dir", "path"}, results: []string{"removed"}},
	"github_pull_request":       {args: []string{"owner", "repo", "head", "base", "title"}, results: []string{"url"}},
	"github_add_review_comment": {args: []string{"owner", "repo", "pull_number", "path", "line"}, results: []string{"draft_count"}},
	"github_submit_review":      {args: []string{"owner", "repo", "pull_number", "event"}, results: []string{"url", "comments"}},
}

// isWorkspaceTool: is name in the ledger?
func isWorkspaceTool(name string) bool {
	_, ok := wsOpSpecs[name]
	return ok
}

// recordWsOp: builds ledger entry for one call/response pair. Failed ops recorded (judge must contradict claims).
func recordWsOp(tool string, args, resp map[string]any) wsOp {
	spec := wsOpSpecs[tool]
	var b strings.Builder
	b.WriteString(tool)
	b.WriteString("(")
	b.WriteString(kvList(args, spec.args))
	b.WriteString(")")
	if errVal, failed := resp["error"]; failed {
		fmt.Fprintf(&b, " → FAILED: %v", errVal)
		return wsOp{tool: tool, detail: b.String()}
	}
	if res := kvList(resp, spec.results); res != "" {
		b.WriteString(" → ")
		b.WriteString(res)
	}
	op := wsOp{tool: tool, detail: b.String()}
	if tool == "read_file" {
		if content, ok := resp["content"].(string); ok && strings.TrimSpace(content) != "" {
			op.sample = strings.TrimSpace(trimToSample(content))
		}
	}
	return op
}

// kvList: renders named keys as "k=v, k=v" (declaration order, string values quoted).
func kvList(m map[string]any, keys []string) string {
	var parts []string
	for _, k := range keys {
		v, ok := m[k]
		if !ok || v == nil {
			continue
		}
		if s, isStr := v.(string); isStr {
			if strings.TrimSpace(s) == "" {
				continue
			}
			parts = append(parts, fmt.Sprintf("%s=%q", k, s))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	return strings.Join(parts, ", ")
}

// maxLedgerOps: caps operations in buildWorkspaceSection, keeping the tail (commit, final test run).
const maxLedgerOps = 80

// buildWorkspaceSection: renders workspace ledger for prompt (judge and revise). Empty when no ops.
func buildWorkspaceSection(act workerActivity) string {
	if len(act.workspace) == 0 {
		return ""
	}
	ops := act.workspace
	omitted := 0
	if len(ops) > maxLedgerOps {
		omitted = len(ops) - maxLedgerOps
		ops = ops[omitted:]
	}
	var sb strings.Builder
	sb.WriteString("Workspace activity (operations the worker actually performed - do not contradict this; " +
		"any operation or outcome the answer claims that is NOT listed here did not happen):\n")
	if omitted > 0 {
		fmt.Fprintf(&sb, "  (… %d earlier operation(s) omitted)\n", omitted)
	}
	for _, op := range ops {
		sb.WriteString("  • ")
		sb.WriteString(op.detail)
		sb.WriteString("\n")
		if op.sample != "" {
			sb.WriteString("      content sample: ")
			sb.WriteString(strings.ReplaceAll(op.sample, "\n", "\\n"))
			sb.WriteString("\n")
		}
	}
	return sb.String()
}
