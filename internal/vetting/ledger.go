// The workspace activity ledger: which fs/git/run_command operations the
// worker ACTUALLY performed, reconstructed from session events and shown to
// the judge, so an answer's claims ("committed as abc123", "the README says
// …") are checkable against ground truth instead of taken on confidence.
// The coder's analog of RequireRetrieval — motivated by a live e2e where a
// fabricated commit + fabricated README quotes sailed past a judge that
// could only see web activity.
package vetting

import (
	"encoding/json"
	"fmt"
	"reflect"
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
	"git_checkout":        {args: []string{"dir", "ref"}, results: []string{"branch", "head"}},
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

	// The delivery step (GitHub App extension, internal/github): the PR URL is
	// exactly the kind of outcome an answer claims, so it belongs in the ledger
	// the judge checks claims against — and its success feeds the deterministic
	// delivery check (delivery.go).
	"github_pull_request": {args: []string{"owner", "repo", "head", "base", "title"}, results: []string{"url"}},

	// The reviewer's delivery (same extension): drafting an inline comment and
	// SUBMITTING the review. "I reviewed the PR" is a claim like any other — the
	// ledger is what contradicts it — and the submit feeds the deterministic
	// review check (delivery.go).
	"github_add_review_comment": {args: []string{"owner", "repo", "pull_number", "path", "line"}, results: []string{"draft_count"}},
	"github_submit_review":      {args: []string{"owner", "repo", "pull_number", "event"}, results: []string{"url", "comments"}},
}

// isWorkspaceTool reports whether name belongs in the workspace ledger.
func isWorkspaceTool(name string) bool {
	_, ok := wsOpSpecs[name]
	return ok
}

// RunCodeToolName is code mode's one tool (internal/tools/run_code.go): the
// model writes a program, the program calls tools as ordinary functions, and ONE
// result comes back.
//
// It is declared HERE, in vetting, and imported by tools — never the other way
// round (tools already depends on vetting; the reverse would be a cycle) — and
// it is the ledger's business as much as the registry's. A tool called from
// inside a script emits NO session event, so this scanner would be blind to it:
// a script that wrote files and committed them would look, to the trust gate,
// like a node claiming work it never did. run_code's response therefore carries
// a `calls` record of every in-script call, and activityFromSessionAt EXPANDS it
// through the very same recording path a direct call takes (see
// activityScanner.apply in node.go). After that expansion a file written from
// inside a script is indistinguishable, to the gate, from one written by a
// direct write_file call — which is exactly the property that keeps code mode
// from punching a hole in the trust gate.
const RunCodeToolName = "run_code"

// The keys of one entry in run_code's `calls` record. Written by
// internal/tools/run_code.go (runCodeCall), read by expandRunCode.
const (
	runCodeCallName   = "name"
	runCodeCallArgs   = "args"
	runCodeCallResult = "result"
)

// innerCall is one tool call a script made, recovered from a run_code response.
type innerCall struct {
	name   string
	args   map[string]any
	result map[string]any
}

// expandRunCode recovers the in-script calls from a run_code FunctionResponse,
// in the order the script made them. Tolerant of both shapes the record arrives
// in: the live in-memory result ([]any of maps) and the same thing round-tripped
// through the event store's JSON. A response with no record (a malformed or
// pre-feature event) expands to nothing — the ledger simply records no
// operations, which is the safe direction: unevidenced claims fail.
func expandRunCode(resp map[string]any) []innerCall {
	raw, ok := resp[runCodeCallsKey]
	if !ok || raw == nil {
		return nil
	}
	rv := reflect.ValueOf(raw)
	if rv.Kind() != reflect.Slice && rv.Kind() != reflect.Array {
		return nil
	}
	out := make([]innerCall, 0, rv.Len())
	for i := 0; i < rv.Len(); i++ {
		entry, ok := asStringMap(rv.Index(i).Interface())
		if !ok {
			continue
		}
		name, _ := entry[runCodeCallName].(string)
		if name == "" {
			continue
		}
		args, _ := asStringMap(entry[runCodeCallArgs])
		result, _ := asStringMap(entry[runCodeCallResult])
		if args == nil {
			args = map[string]any{}
		}
		if result == nil {
			result = map[string]any{}
		}
		out = append(out, innerCall{name: name, args: args, result: result})
	}
	return out
}

// runCodeCallsKey is the field of run_code's result that carries the record.
const runCodeCallsKey = "calls"

// asStringMap coerces one record entry to a generic object, whether it arrived
// as a map or as a struct the event store has yet to marshal.
func asStringMap(v any) (map[string]any, bool) {
	if v == nil {
		return nil, false
	}
	if m, ok := v.(map[string]any); ok {
		return m, true
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, false
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, false
	}
	return m, true
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
