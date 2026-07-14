// Code mode (internal/tools/run_code.go): instead of emitting ONE tool call per
// turn and waiting — every intermediate result landing in its context — the
// model writes a PROGRAM that calls the tools as ordinary functions. The program
// runs; ONE result comes back. A single turn can read five files, grep, and write
// a patch, with real control flow, and the five file contents never enter the
// model's context at all.
//
// The motivating failure was live: a code-implementer spent 25 minutes and 98
// tool calls re-reading the same files and wrote nothing, because a 65k context
// window cannot hold what it needed to hold.
//
// THE API IS GENERATED, NEVER HAND-WRITTEN. The callable surface comes from each
// bound tool's real Declaration() — name, description, parameter schema — and each
// JS function is bound straight to that same tool object's Run. There is exactly
// one source of truth, so a tool whose schema changes changes here too, in the
// same commit, with no parallel list to drift. run_code_test.go's
// TestNoHandMaintainedToolList enforces that: this file may not name a single
// tool in a string literal.
//
// THE GUARDS STILL HOLD. registry.go assembles this tool LAST, over the tools it
// has ALREADY built and wrapped in the guard ladder (guard.go) and the per-node
// cancel guard (cancelguard.go). A script's call invokes that same wrapped
// runnableTool.Run, so the path jail, the OS sandbox, the safety judge, the
// cancel guard and the workspace caps apply to every in-script call for free. The
// script itself has NO ambient capability whatsoever — no filesystem, no network,
// no exec, no require/import. It can only call the functions bound here. That is a
// STRONGER sandbox than the shell it replaces, not a weaker one.
//
// THE LEDGER STILL SEES IT. A tool called inside a script produces no session
// event, so the trust gate's activity ledger — which scans session events for
// FunctionCall/FunctionResponse pairs — would be blind to it, and a node that
// really did commit code would be failed for claiming work with no evidence. So
// the result carries `calls`: a compact record of every in-script call, which
// vetting's scanner EXPANDS through the exact same recording path a direct call
// takes (see internal/vetting/node.go's activityScanner). A file written from
// inside a script is indistinguishable, to the gate, from one written by a direct
// call.
package tools

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dop251/goja"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/vetting"
)

// The two bounds a runaway script is stopped by. Vars, not consts, only so the
// tests can shrink them; nothing in production writes to them.
var (
	// runCodeTimeout bounds a script's wall clock. A script must never hang a
	// node: goja's Interrupt stops it wherever it is, including inside a
	// `while(true){}` that touches nothing.
	runCodeTimeout = 60 * time.Second
	// runCodeMaxCalls bounds how many tools ONE script may invoke. Generous —
	// the point of the feature is a turn that does a lot — but finite, so a
	// looping script cannot grind forever under the timeout.
	runCodeMaxCalls = 200
)

const (
	// runCodeMaxResult / runCodeMaxLogs cap what comes back into the model's
	// context. Code mode exists to keep bulk OUT of the context; an uncapped
	// return value would put it straight back.
	runCodeMaxResult = 16 << 10
	runCodeMaxLogs   = 8 << 10
	// runCodeSampleChars bounds the one string the compact call record keeps
	// rather than elides: the error message (the ledger reads it to mark an
	// operation FAILED, and the model reads it to fix its script).
	runCodeSampleChars = 300
	// runCodeMaxDescChars bounds how much of each tool's own description is
	// reproduced in the generated API listing. The model already has the tool's
	// FULL description from its own declaration (code mode adds a tool, it never
	// removes one), so the listing needs only enough to identify each function.
	runCodeMaxDescChars = 240
)

var (
	errScriptTimeout   = errors.New("script exceeded its time limit")
	errNoBoundTools    = errors.New("tools: code mode needs at least one other tool to expose as its API")
	errArgsNotAnObject = errors.New("argument must be a single object")
)

func errTooManyCalls() error {
	return fmt.Errorf("script exceeded its limit of %d tool calls", runCodeMaxCalls)
}

type runCodeArgs struct {
	// Code is the script body. See the generated description for the contract.
	Code string `json:"code"`
}

// runCodeCall is ONE in-script tool call, as the model and the trust gate's
// ledger see it: what was called, with what, and the SMALL fields of what came
// back. The bulky payloads a script consumes (file contents, grep matches,
// command output) are elided here — putting them back in the context is exactly
// what code mode exists to avoid. internal/vetting/node.go reads these keys.
type runCodeCall struct {
	Name   string         `json:"name"`
	Args   map[string]any `json:"args"`
	Result map[string]any `json:"result"`
}

// runCodeResult is the ONE tool result a whole program comes back as.
type runCodeResult struct {
	// Result is the script's return value, JSON-encoded and capped.
	Result string `json:"result,omitempty"`
	// Logs is everything the script printed (console.log), capped.
	Logs []string `json:"logs,omitempty"`
	// Calls is the compact record of every tool the script invoked — in order,
	// including the ones that failed. It is what the trust gate's ledger expands.
	Calls []runCodeCall `json:"calls"`
	// Error is the script's own failure (a syntax error, an uncaught exception, a
	// timeout), with the line where goja could give one. Populated INSTEAD of a Go
	// error so that Calls survives: a script that wrote a file and then threw did
	// really write that file, and the ledger must still see it.
	Error string `json:"error,omitempty"`
}

// newRunCode builds the code-mode tool over `bound` — the tools registry.Build
// has already constructed and wrapped. exclude drops a tool from the in-script
// API without removing it from the agent (see registry.go: confirm-tier and
// long-running tools stay direct-call only, because a mid-script human pause has
// nowhere to suspend to).
func newRunCode(bound []tool.Tool, exclude func(tool.Tool) bool) (tool.Tool, error) {
	api := map[string]runnableTool{}
	var decls []*genai.FunctionDeclaration
	for _, t := range bound {
		if t == nil || t.Name() == vetting.RunCodeToolName || (exclude != nil && exclude(t)) {
			continue
		}
		rt, ok := t.(runnableTool)
		if !ok {
			continue // not a function tool: nothing to bind, nothing to declare
		}
		decl := rt.Declaration()
		if decl == nil {
			continue
		}
		api[t.Name()] = rt
		decls = append(decls, decl)
	}
	if len(api) == 0 {
		return nil, errNoBoundTools
	}
	sort.Slice(decls, func(i, j int) bool { return decls[i].Name < decls[j].Name })
	return functiontool.New[runCodeArgs, runCodeResult](
		functiontool.Config{Name: vetting.RunCodeToolName, Description: runCodeDescription(decls)},
		func(ctx agent.Context, a runCodeArgs) (runCodeResult, error) {
			return runScript(ctx, api, a.Code), nil
		},
	)
}

// ---------------------------------------------------------------------------
// The interpreter
// ---------------------------------------------------------------------------

// scriptRun is one execution's bookkeeping: the call record the ledger will
// expand, and the captured logs. Not shared across runs — a fresh goja VM and a
// fresh scriptRun per call, so two scripts can never see each other's state.
type scriptRun struct {
	calls    []runCodeCall
	logs     []string
	logBytes int
}

// runScript compiles and runs one script in a fresh goja VM. It NEVER returns a
// Go error: every failure comes back inside runCodeResult, so the calls the
// script already made survive into the ledger and so the model gets an
// actionable message it can fix its own script from.
func runScript(ctx agent.Context, api map[string]runnableTool, code string) (out runCodeResult) {
	r := &scriptRun{}
	vm := goja.New()

	defer func() {
		// goja turns our panics into JS exceptions, so nothing should reach here —
		// but a script must not be able to take a node down with it.
		if rec := recover(); rec != nil {
			out = r.result()
			out.Error = fmt.Sprintf("script aborted: %v", rec)
		}
	}()

	r.bind(ctx, vm, api)

	// Wall clock. Interrupt is the one thing that stops a script touching nothing
	// (`while(true){}`); a timeout in the tool's own context would not.
	timer := time.AfterFunc(runCodeTimeout, func() { vm.Interrupt(errScriptTimeout) })
	defer timer.Stop()
	// The node's own cancellation stops the script too — the cancel guard refuses
	// each individual call, but a script between calls would not notice.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			vm.Interrupt(ctx.Err())
		case <-done:
		}
	}()

	// The script is the BODY of a function, so `return` works — the idiom every
	// model already writes. The prefix stays on line 1 so goja's reported line
	// numbers are the model's own line numbers.
	v, err := vm.RunString("(function(){" + code + "\n})()")
	out = r.result()
	if err != nil {
		out.Error = scriptError(err)
		return out
	}
	out.Result = encodeReturn(v)
	return out
}

// bind installs the script's ENTIRE capability set: one JS function per tool,
// named exactly as the tool is named (no mapping layer = nothing to drift), plus
// console. Everything else a script might reach for — fetch, require, process,
// the filesystem — simply does not exist in a bare goja VM.
func (r *scriptRun) bind(ctx agent.Context, vm *goja.Runtime, api map[string]runnableTool) {
	for name, t := range api {
		_ = vm.Set(name, r.jsFunc(ctx, vm, name, t))
	}

	console := vm.NewObject()
	logFn := func(call goja.FunctionCall) goja.Value {
		parts := make([]string, 0, len(call.Arguments))
		for _, a := range call.Arguments {
			parts = append(parts, displayValue(a))
		}
		r.appendLog(strings.Join(parts, " "))
		return goja.Undefined()
	}
	for _, m := range []string{"log", "info", "warn", "error", "debug"} {
		_ = console.Set(m, logFn)
	}
	_ = vm.Set("console", console)
}

// jsFunc is the bridge: a JS call becomes a real, fully-guarded tool invocation.
// The SCRIPT gets the tool's full result (it needs the file's content to do its
// job); the model and the ledger get the compact record. That asymmetry IS code
// mode.
//
// A failing call THROWS a catchable JS exception, so a script can handle partial
// failure itself instead of dying — and the failure is recorded either way.
func (r *scriptRun) jsFunc(ctx agent.Context, vm *goja.Runtime, name string, t runnableTool) func(goja.FunctionCall) goja.Value {
	return func(call goja.FunctionCall) goja.Value {
		args, err := exportArgs(call)
		if err != nil {
			panic(vm.NewTypeError("%s: %s, e.g. %s({ ... })", name, err.Error(), name))
		}
		if len(r.calls) >= runCodeMaxCalls {
			// Interrupt, not just throw: a throw is catchable, and a script that
			// catches its own call-cap inside a loop would never stop.
			capped := errTooManyCalls()
			vm.Interrupt(capped)
			panic(vm.NewGoError(capped))
		}
		res, err := t.Run(ctx, args)
		if err != nil {
			// Recorded, not dropped — "the tests passed" claimed over a failed run
			// must stay contradictable. The shape matches what ADK writes into a
			// direct call's FunctionResponse on error, which is what the ledger reads.
			r.calls = append(r.calls, runCodeCall{Name: name, Args: args, Result: map[string]any{"error": err.Error()}})
			if strings.Contains(err.Error(), cancelledMsg) {
				// The user stopped this node. A catch block must not be able to keep
				// the script going.
				vm.Interrupt(err)
			}
			panic(vm.NewGoError(err))
		}
		r.calls = append(r.calls, runCodeCall{Name: name, Args: args, Result: compactResult(res)})
		return vm.ToValue(res)
	}
}

// exportArgs takes the ONE object argument every bound tool's JS signature has —
// the tools' parameters are JSON objects, so a positional signature would need a
// hand-maintained field order, which is precisely the drift this design forbids.
func exportArgs(call goja.FunctionCall) (map[string]any, error) {
	if len(call.Arguments) == 0 {
		return map[string]any{}, nil
	}
	v := call.Arguments[0]
	if goja.IsUndefined(v) || goja.IsNull(v) {
		return map[string]any{}, nil
	}
	m, ok := v.Export().(map[string]any)
	if !ok {
		return nil, errArgsNotAnObject
	}
	return m, nil
}

func (r *scriptRun) appendLog(s string) {
	if r.logBytes >= runCodeMaxLogs {
		return
	}
	if len(s) > runCodeMaxLogs-r.logBytes {
		s = truncate(s, runCodeMaxLogs-r.logBytes) + " …(log truncated)"
	}
	r.logBytes += len(s)
	r.logs = append(r.logs, s)
}

// result snapshots what the script did, whatever happened to it. Calls is never
// nil: an empty list is a positive statement ("this script touched nothing"),
// which is different from an absent one.
func (r *scriptRun) result() runCodeResult {
	calls := r.calls
	if calls == nil {
		calls = []runCodeCall{}
	}
	return runCodeResult{Calls: calls, Logs: r.logs}
}

// scriptError renders a goja failure as something the model can act on: the
// exception's message and stack (which carries the line number), the syntax
// error's position, or the reason a run was interrupted.
func scriptError(err error) string {
	var interrupted *goja.InterruptedError
	if errors.As(err, &interrupted) {
		return fmt.Sprintf("script stopped: %v", interrupted.Value())
	}
	var ex *goja.Exception
	if errors.As(err, &ex) {
		return strings.TrimSpace(ex.String())
	}
	return err.Error()
}

// encodeReturn JSON-encodes the script's return value, capped. undefined/null —
// a script that only logged, or only wrote files — encodes to nothing rather
// than to the string "null".
func encodeReturn(v goja.Value) string {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return ""
	}
	b, err := json.Marshal(v.Export())
	if err != nil {
		return truncate(fmt.Sprint(v.Export()), runCodeMaxResult)
	}
	if len(b) > runCodeMaxResult {
		return truncate(string(b), runCodeMaxResult) + " …(result truncated; return less)"
	}
	return string(b)
}

// displayValue renders one console.log argument: objects as JSON (a model that
// logs a result wants to see it, not "[object Object]"), everything else as JS
// would.
func displayValue(v goja.Value) string {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return v.String()
	}
	switch v.ExportType().Kind() {
	case reflect.Map, reflect.Slice, reflect.Array, reflect.Struct:
		if b, err := json.Marshal(v.Export()); err == nil {
			return string(b)
		}
	}
	return v.String()
}

// ---------------------------------------------------------------------------
// The compact call record
// ---------------------------------------------------------------------------

// payloadKeys are the fields a tool returns for a SCRIPT to consume: the file's
// text, the command's output, the grep's matches, the directory's entries. They
// are precisely what must NOT come back into the model's context — reproducing
// them in the call record would put the whole point of code mode back where it
// started. Each is replaced by its SIZE, so the model still knows what it did not
// see.
//
// These are FIELD names, not tool names: nothing here needs to change when a tool
// is added or renamed, and the size fallback in compactResult catches a bulky
// field no one thought to name here, so a new tool cannot silently reintroduce
// the leak.
var payloadKeys = map[string]bool{
	"content": true, "output": true, "stdout": true, "stderr": true,
	"matches": true, "entries": true, "paths": true, "results": true,
	"result": true, "diff": true, "instructions": true, "skills": true,
}

// errorKey is the one string field the ledger's failure detection reads
// (recordWsOp treats its presence as FAILED), so it is always kept — truncated,
// never elided, never renamed.
const errorKey = "error"

// maxKeptString is the size fallback: any string field not named above that is
// longer than this is bulk, whatever it is called, and is elided to its length.
const maxKeptString = 200

// compactResult builds the record of ONE in-script call as the model and the
// trust gate see it: the small, claim-bearing fields (error, dir, head, sha,
// exit_code, bytes, created, …) kept verbatim, the bulky payloads elided to a
// size or a count.
//
// This is what bounds code mode's context cost, and it is the one place the
// feature's central claim is made true: the script sees the full result — it must,
// that is its job — and the model never does.
func compactResult(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch {
		case k == errorKey:
			if s, ok := v.(string); ok {
				out[k] = truncate(s, runCodeSampleChars)
				continue
			}
			out[k] = v
		case payloadKeys[k]:
			key, size := elide(k, v)
			out[key] = size
		case isBulkString(v):
			key, size := elide(k, v)
			out[key] = size
		case isList(v):
			out[k+"_count"] = reflect.ValueOf(v).Len()
		default:
			if nested, ok := v.(map[string]any); ok {
				out[k] = compactResult(nested)
				continue
			}
			out[k] = v
		}
	}
	return out
}

// elide replaces a payload with its size. A field that is NOT actually bulky
// (a payload key that happened to hold a bool or a number) is kept as it is —
// eliding it would lose a fact for nothing.
func elide(k string, v any) (string, any) {
	if s, ok := v.(string); ok {
		return k + "_chars", len(s)
	}
	if isList(v) {
		return k + "_count", reflect.ValueOf(v).Len()
	}
	return k, v
}

func isBulkString(v any) bool {
	s, ok := v.(string)
	return ok && utf8.RuneCountInString(s) > maxKeptString
}

func isList(v any) bool {
	rv := reflect.ValueOf(v)
	return rv.IsValid() && (rv.Kind() == reflect.Slice || rv.Kind() == reflect.Array)
}

// truncate cuts s to at most n runes, on a valid UTF-8 boundary.
func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// ---------------------------------------------------------------------------
// The generated API listing — run_code's description IS the API
// ---------------------------------------------------------------------------

// runCodeDescription renders the tool's description from the bound tools' real
// declarations. Nothing here is hand-written per tool: the names, the parameter
// names, their types and their required-ness all come from the schema ADK
// inferred from the tool's own Go argument struct.
func runCodeDescription(decls []*genai.FunctionDeclaration) string {
	var b strings.Builder
	b.WriteString(runCodePreamble())
	b.WriteString("\n\nAVAILABLE FUNCTIONS (this list is generated from the tools themselves — it cannot go stale):\n")
	for _, d := range decls {
		fmt.Fprintf(&b, "\n%s(%s) -> %s\n", d.Name, renderParams(d.ParametersJsonSchema), renderReturn(d.ResponseJsonSchema))
		if s := summarize1(d.Description); s != "" {
			fmt.Fprintf(&b, "    %s\n", s)
		}
	}
	return b.String()
}

// runCodePreamble is the only prose in the description — the contract, not the
// API. Every function name in the listing above is generated.
func runCodePreamble() string {
	return fmt.Sprintf(`Write a JavaScript program that calls your tools as ordinary functions; it runs; ONE result comes back.

Use this INSTEAD of a long chain of one-tool-per-turn calls. A single script can read five files, grep, and write a patch — with real loops and conditionals — and the bulk (file contents, grep matches, command output) is processed INSIDE the script and never enters your context. That is the whole point: reach for it whenever you would otherwise make several related calls in a row, or when what you need to look at is bigger than what you can hold.

THE CONTRACT
  - Your code is the BODY of a function. Use `+"`return`"+` to return a value.
  - Every tool below is a plain SYNCHRONOUS function in scope. Each takes ONE object argument and returns its result object directly. No await, no promises, no callbacks.
  - `+"`console.log(...)`"+` is captured and returned to you.
  - A failing tool call THROWS. Wrap a call in try/catch to handle partial failure and still return something useful; an uncaught throw ends the script (you still get its logs, its calls so far, and the error with its line number).
  - The script has NO other capability: no filesystem, no network, no require/import, no process. It can call the functions below and nothing else.
  - Limits: %s wall clock, %d tool calls, and the returned value is capped — return a SUMMARY, not a dump.

WHAT YOU GET BACK
  result — your return value (JSON). logs — what you printed. calls — a compact record of each tool call you made (bulky payloads elided; this is also what the trust gate audits your work by). error — set if the script threw.

EXAMPLE
  const hits = grep({ pattern: "func Build", path: "internal" });
  const files = [...new Set(hits.matches.map(m => m.path))].slice(0, 5);
  const sizes = {};
  for (const f of files) {
    try {
      sizes[f] = read_file({ path: f }).content.split("\n").length;
    } catch (e) {
      sizes[f] = "unreadable: " + e.message;
    }
  }
  return { files, sizes };

Tools that need a human's approval, and long-running tools, are NOT in the list below — a script has nowhere to pause. Call those directly, as you always have. Every tool you have remains callable the normal way; this is an addition, not a replacement.`, runCodeTimeout, runCodeMaxCalls)
}

// renderParams turns a tool's parameter schema into a JS-ish object signature —
// `{ path: string, start_line?: integer }`. The schema is walked as generic JSON
// so this survives ADK swapping its schema type.
func renderParams(schema any) string {
	m := asMap(schema)
	if m == nil {
		return ""
	}
	props := asMap(m["properties"])
	if len(props) == 0 {
		return ""
	}
	required := map[string]bool{}
	if req, ok := m["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				required[s] = true
			}
		}
	}
	names := make([]string, 0, len(props))
	for k := range props {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		opt := ""
		if !required[n] {
			opt = "?"
		}
		parts = append(parts, fmt.Sprintf("%s%s: %s", n, opt, typeName(props[n], 0)))
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

// renderReturn names the shape a tool hands back, so a script's author knows
// which fields to reach for.
func renderReturn(schema any) string {
	if t := typeName(schema, 0); t != "" && t != "any" {
		return t
	}
	return "object"
}

// typeName renders a JSON-schema node as a JS type. depth bounds the recursion
// into nested objects: one level of field names is what a script's author needs;
// deeper than that, "object" is the honest answer and the tool's own declaration
// carries the detail.
func typeName(schema any, depth int) string {
	m := asMap(schema)
	if m == nil {
		return "any"
	}
	switch t := schemaType(m); t {
	case "array":
		return typeName(m["items"], depth+1) + "[]"
	case "object", "":
		props := asMap(m["properties"])
		if len(props) == 0 {
			if t == "object" {
				return "object"
			}
			return "any"
		}
		if depth >= 1 {
			return "object"
		}
		names := make([]string, 0, len(props))
		for k := range props {
			names = append(names, k)
		}
		sort.Strings(names)
		parts := make([]string, 0, len(names))
		for _, n := range names {
			parts = append(parts, fmt.Sprintf("%s: %s", n, typeName(props[n], depth+1)))
		}
		return "{ " + strings.Join(parts, ", ") + " }"
	default:
		return t
	}
}

// schemaType reads a node's type. An OPTIONAL field of a nullable Go kind (a
// slice, a pointer) comes through as a union — `"type": ["null", "array"]` — so
// a plain string read would have quietly called every one of them "any", which is
// exactly the sort of silently-degraded API listing this feature cannot afford.
func schemaType(m map[string]any) string {
	switch t := m["type"].(type) {
	case string:
		return t
	case []any:
		for _, v := range t {
			if s, ok := v.(string); ok && s != "null" {
				return s
			}
		}
	}
	return ""
}

// asMap re-reads any schema value as a generic JSON object. ADK's declaration
// carries whatever type its schema library uses; a JSON round-trip is the one
// view of it that cannot break.
func asMap(v any) map[string]any {
	if v == nil {
		return nil
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}

// summarize1 takes a tool description's first paragraph, bounded. The model
// already holds each tool's FULL description (code mode ADDS a tool, it removes
// none), so the listing only has to say which function is which.
func summarize1(desc string) string {
	desc = strings.TrimSpace(desc)
	if desc == "" {
		return ""
	}
	if i := strings.Index(desc, "\n\n"); i > 0 {
		desc = desc[:i]
	}
	desc = strings.Join(strings.Fields(desc), " ")
	if utf8.RuneCountInString(desc) > runCodeMaxDescChars {
		desc = truncate(desc, runCodeMaxDescChars) + "…"
	}
	return desc
}
