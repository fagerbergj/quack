// The workspace activity ledger: which fs/git/run_command operations the
// worker ACTUALLY performed, reconstructed from session events and shown to
// the judge, so an answer's claims ("committed as abc123", "the README says
// …") are checkable against ground truth instead of taken on confidence.
// The coder's analog of RequireRetrieval — motivated by a live e2e where a
// fabricated commit + fabricated README quotes sailed past a judge that
// could only see web activity.
package vetting

import (
	"fmt"
	"strings"
)

// wsOpSpec names which call args and result fields summarize one workspace
// tool in the ledger. Args identify WHAT the operation targeted; results
// carry the outcome fields a claim would cite (sha for a commit, branch for
// a branch, exit_code for a command). Tools not in this map (web/search/
// memory/HITL tools) are handled by the existing retrieval bookkeeping and
// stay out of the workspace ledger.
type wsOpSpec struct {
	args    []string
	results []string
}

// wsOpSpecs covers every workspace tool an answer could claim an outcome
// from. read_file additionally retains a content sample (handled specially
// in recordWsOp) for quote spot-checking.
var wsOpSpecs = map[string]wsOpSpec{
	"read_file":   {args: []string{"path"}},
	"write_file":  {args: []string{"path"}, results: []string{"bytes", "created"}},
	"edit_file":   {args: []string{"path"}, results: []string{"replacements"}},
	"delete_path": {args: []string{"path"}, results: []string{"deleted"}},
	"run_command": {args: []string{"dir", "command"}, results: []string{"exit_code"}},

	"git_clone":           {args: []string{"url", "dir"}, results: []string{"dir", "head", "default_branch"}},
	"git_status":          {args: []string{"dir"}, results: []string{"branch", "clean"}},
	"git_diff":            {args: []string{"dir", "ref", "path"}},
	"git_log":             {args: []string{"dir"}},
	"git_commit":          {args: []string{"dir", "message"}, results: []string{"sha", "files_changed"}},
	"git_branch":          {args: []string{"dir", "name"}, results: []string{"current"}},
	"git_push":            {args: []string{"dir"}, results: []string{"remote", "branch", "sha"}},
	"git_pull":            {args: []string{"dir"}, results: []string{"branch", "sha", "updated"}},
	"git_rebase":          {args: []string{"dir", "onto"}, results: []string{"sha", "rebased"}},
	"git_worktree_create": {args: []string{"dir", "branch"}, results: []string{"path"}},
	"git_worktree_remove": {args: []string{"dir", "path"}, results: []string{"removed"}},
}

// isWorkspaceTool reports whether name belongs in the workspace ledger.
func isWorkspaceTool(name string) bool {
	_, ok := wsOpSpecs[name]
	return ok
}

// recordWsOp builds the ledger entry for one completed call/response pair.
// An "error" key in the response marks the operation FAILED — recorded, not
// dropped, because "I ran the tests" claimed over a failed run is exactly the
// kind of claim the judge must be able to contradict. read_file's returned
// content is sampled (trimToSample) for quote spot-checks.
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

// kvList renders the named keys present in m as "k=v, k=v" (declaration
// order; absent keys skipped). String values are quoted so a commit message
// or command with spaces reads unambiguously.
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

// maxLedgerOps caps how many operations buildWorkspaceSection renders,
// keeping the TAIL — a long coding session front-loads reads/greps, while the
// operations claims hinge on (the commit, the final test run) come last.
const maxLedgerOps = 80

// buildWorkspaceSection renders the workspace ledger for a prompt (judge and
// revise contexts). Empty when the worker performed no workspace operations —
// web-research nodes see no change at all.
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
	sb.WriteString("Workspace activity (operations the worker actually performed — do not contradict this; " +
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
