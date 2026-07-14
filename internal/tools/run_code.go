// Code mode: the model writes a PROGRAM that calls its tools as ordinary
// functions; the program runs; ONE result comes back. The bulk (file contents,
// grep matches, command output) is consumed inside the script and never enters
// the model's context.
//
// Invariants:
//   - The API is GENERATED from each bound tool's real Declaration(); this file
//     may not name a tool in a string literal (TestNoHandMaintainedToolList).
//   - The guard is on the SCRIPT: run_code's guard tier is raised to the union of
//     its bound tools' tiers (registry.go), the whole program is judged/approved
//     once before it runs, and in-script calls skip their individual guards. The
//     path jail, workspace caps, OS sandbox, and cancel guard still apply per call.
//   - The script has no ambient capability — only the bound functions.
//   - The ledger still sees it: in-script calls produce no session events, so the
//     result carries `calls`, which vetting's activityScanner expands through the
//     same recording path a direct call takes.
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

// Bounds on a runaway script. Vars, not consts, only so tests can shrink them.
var (
	// goja's Interrupt is what stops a `while(true){}` that touches nothing.
	runCodeTimeout  = 60 * time.Second
	runCodeMaxCalls = 200 // tool calls per script
)

const (
	// Caps on what comes back into the model's context.
	runCodeMaxResult = 16 << 10
	runCodeMaxLogs   = 8 << 10
	// runCodeSampleChars bounds the error message — the one string the compact
	// record keeps verbatim (the ledger reads it to mark an operation FAILED).
	runCodeSampleChars = 300
	// Per-tool description budget in the generated API listing; the model already
	// has each tool's full description from its own declaration.
	runCodeMaxDescChars = 240
)

// Echo-detector thresholds (see echoWarning). The question is never "is the
// return value big" but "how much of it is verbatim payload the tools already
// handed the script".
const (
	// Shortest returned string worth testing — below this a match means nothing
	// (every source file contains "func") and a useful quote lives here.
	runCodeEchoMinChunk = 64
	// Verbatim payload in one return value past which a quote is a dump.
	runCodeEchoWarnBytes = 4 << 10
	// Size at which ONE verbatim returned string is ELIDED rather than delivered.
	// Enforced, not just warned about: the first live script returned 52 KB of
	// file contents despite a capitalised prohibition with a worked example.
	runCodeEchoElideChunk = 1 << 10
	// Bound on what the detector RETAINS to compare against — a script reading
	// 200 files must not make the tool hold all of them. Past it, bytes are
	// still counted, just no longer matchable.
	runCodeEchoScanBytes = 4 << 20
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
	// Warning is set when the script RETURNED the bulk it was supposed to keep out
	// of the context — see echoWarning. It is addressed to the model, in the result,
	// because that is the only place the model will read it in time to do better.
	Warning string `json:"warning,omitempty"`
}

// newRunCode builds the code-mode tool over `bound` — the SCRIPT view of the tools
// registry.Build has already constructed (cancel-guarded; the guard ladder sits on
// run_code itself). exclude drops a tool from the in-script API without removing it
// from the agent — only the turn-ending, long-running ones (registry.go's
// noCodeMode), which have no turn boundary inside a script to be answered on.
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
	// payloadBytes is how many bytes of BULK (file contents, command output, grep
	// matches — the fields compactResult elides) the tools handed this script, and
	// payloads is as much of that text as the echo detector may retain to compare
	// the return value against. See echoWarning.
	payloadBytes    int
	payloads        []string
	payloadRetained int
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
	out.Result, out.Warning = r.encodeReturn(v)
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
		r.recordPayload(res)
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
		return exceptionMessage(ex)
	}
	return err.Error()
}

// exceptionMessage renders a thrown exception as the model needs it: the message,
// plainly, and the line in ITS OWN script that threw. goja's String() buries that
// under a Go stack trace of native frames the model cannot act on.
func exceptionMessage(ex *goja.Exception) string {
	lines := strings.Split(strings.TrimSpace(ex.String()), "\n")
	msg := strings.TrimPrefix(strings.TrimSpace(lines[0]), "GoError: ")
	if n := scriptLine(lines[1:]); n != "" {
		return msg + " (at line " + n + " of your script)"
	}
	return msg
}

// scriptLine finds the first stack frame that is in the SCRIPT (goja calls it
// "<eval>") and returns its line number — the only part of the trace the model can
// use. "" when the throw has no script frame at all.
func scriptLine(stack []string) string {
	const marker = "<eval>:"
	for _, f := range stack {
		i := strings.Index(f, marker)
		if i < 0 {
			continue
		}
		rest := f[i+len(marker):]
		if j := strings.IndexAny(rest, ":( "); j > 0 {
			return rest[:j]
		}
	}
	return ""
}

// encodeReturn JSON-encodes the script's return value, capped, and checks it for
// the one mistake that makes code mode pointless (echoWarning). undefined/null —
// a script that only logged, or only wrote files — encodes to nothing rather
// than to the string "null".
func (r *scriptRun) encodeReturn(v goja.Value) (result, warning string) {
	if v == nil || goja.IsUndefined(v) || goja.IsNull(v) {
		return "", ""
	}
	exported := v.Export()
	// Elide BEFORE encoding: the bytes only cost the model anything when they are
	// handed back, so this is the boundary where the feature is actually enforced.
	exported, cut := r.elideEchoes(exported)
	warning = r.echoWarning(cut)
	b, err := json.Marshal(exported)
	if err != nil {
		return truncate(fmt.Sprint(exported), runCodeMaxResult), warning
	}
	if len(b) > runCodeMaxResult {
		return truncate(string(b), runCodeMaxResult) + " …(result truncated; return less)", warning
	}
	return string(b), warning
}

// ---------------------------------------------------------------------------
// The echo detector — the model handing the payload back to itself
// ---------------------------------------------------------------------------

// echoWarning tells the model its return value echoed tool payload. An echo is
// PROVED, not guessed: a returned string is an echo only when it is verbatim (a
// substring or superstring of) a payload some tool in this very script handed
// back — so a script that COMPUTES a large answer is never warned at, however
// big. The warning must be in the result, because that is the only place the
// model will read it in time to do better.
func (r *scriptRun) echoWarning(elided int) string {
	if elided == 0 {
		return ""
	}
	return fmt.Sprintf("YOU TRIED TO RETURN THE FILE CONTENTS, AND I DROPPED THEM. %s of your return value "+
		"was text the tools in this script had already handed you verbatim (they returned %s of such payload "+
		"in total), so it was ELIDED rather than delivered — it is not in your context. That is the entire "+
		"cost code mode exists to avoid: the SCRIPT reads the bulk, YOU do not. Return only the structure you "+
		"need — paths, line numbers, counts, symbol names, a short quoted snippet — never a whole "+
		"`content`/`output`/`matches` field. {path, total_lines, exports: [...]} is right; {path, content} is "+
		"the mistake you just made. If you genuinely need a file's text, read_file it directly.",
		humanBytes(elided), humanBytes(r.payloadBytes))
}

// elideEchoes rebuilds the return value with every LARGE verbatim-payload string
// replaced by a marker naming what was dropped, and reports how many bytes it cut.
// A computed answer is never touched: a patch, a diff, a generated file is not a
// verbatim substring of anything a tool returned — its own markers and interleaving
// break containment — so only an actual echo matches. A short quote is not touched
// either (runCodeEchoElideChunk).
func (r *scriptRun) elideEchoes(returned any) (any, int) {
	cut := 0
	out := mapStrings(returned, 0, func(s string) (string, bool) {
		if len(s) < runCodeEchoElideChunk || !r.isEcho(s) {
			return s, false
		}
		cut += len(s)
		return fmt.Sprintf("[elided by code mode: %s of content your script already read. "+
			"It is NOT re-sent to you — that is the whole point. Return structure instead: "+
			"paths, line numbers, counts, symbol names, a short quote.]", humanBytes(len(s))), true
	})
	return out, cut
}

// isEcho reports whether s is verbatim payload some tool in this script returned.
func (r *scriptRun) isEcho(s string) bool {
	for _, p := range r.payloads {
		if strings.Contains(p, s) || strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// echoedBytes is how many bytes of the return value are verbatim tool payload.
// Strings shorter than runCodeEchoMinChunk are not tested: a short quote is a
// legitimate answer, and a short string matches by coincidence.
func (r *scriptRun) echoedBytes(returned any) int {
	total := 0
	walkStrings(returned, 0, func(s string) {
		if len(s) < runCodeEchoMinChunk {
			return
		}
		for _, p := range r.payloads {
			// Either direction is an echo: the return may quote a slice of a file, or
			// wrap a whole file inside a bigger string.
			if strings.Contains(p, s) || strings.Contains(s, p) {
				total += len(s)
				return
			}
		}
	})
	return total
}

// recordPayload accounts for the bulk ONE tool call handed the script — the same
// fields compactResult elides from the model's view, which is exactly the set the
// model must not get back by another route.
func (r *scriptRun) recordPayload(res map[string]any) {
	for k, v := range res {
		switch {
		case payloadKeys[k], isBulkString(v):
			walkStrings(v, 0, func(s string) {
				r.payloadBytes += len(s)
				if len(s) < runCodeEchoMinChunk || r.payloadRetained >= runCodeEchoScanBytes {
					return
				}
				r.payloads = append(r.payloads, s)
				r.payloadRetained += len(s)
			})
		default:
			if nested, ok := v.(map[string]any); ok {
				r.recordPayload(nested)
			}
		}
	}
}

// maxWalkDepth bounds walkStrings. A goja object can be cyclic, and a tool result
// is arbitrarily nested JSON; neither may hang a node.
const maxWalkDepth = 16

// walkStrings visits every string inside an arbitrary decoded JSON/JS value.
func walkStrings(v any, depth int, fn func(string)) {
	if v == nil || depth > maxWalkDepth {
		return
	}
	if s, ok := v.(string); ok {
		fn(s)
		return
	}
	rv := reflect.ValueOf(v)
	if !rv.IsValid() {
		return
	}
	switch rv.Kind() {
	case reflect.Slice, reflect.Array:
		for i := 0; i < rv.Len(); i++ {
			walkStrings(rv.Index(i).Interface(), depth+1, fn)
		}
	case reflect.Map:
		for _, k := range rv.MapKeys() {
			walkStrings(rv.MapIndex(k).Interface(), depth+1, fn)
		}
	case reflect.Ptr, reflect.Interface:
		if !rv.IsNil() {
			walkStrings(rv.Elem().Interface(), depth+1, fn)
		}
	}
}

// humanBytes renders a size the way the warning needs to land: "12.4 KB", not
// "12683".
func humanBytes(n int) string {
	if n < 1<<10 {
		return fmt.Sprintf("%d bytes", n)
	}
	return fmt.Sprintf("%.1f KB", float64(n)/1024)
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

// payloadKeys are the bulk fields elided from the call record (replaced by their
// SIZE, so the model knows what it did not see). FIELD names, not tool names —
// nothing changes when a tool is added, and compactResult's size fallback catches
// a bulky field no one named here, so a new tool cannot reintroduce the leak.
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

RETURN ONLY WHAT YOU NEED — NEVER THE FILE CONTENTS
  The SCRIPT reads the bulk. YOU do not. Your return value is the ONE thing that comes back into your context, so it must carry the structure you extracted, never the payload you extracted it from. This is not a style note: returning the contents defeats the entire tool, and costs you exactly the context you called it to save.

    WRONG — you have now read the file into your context, the long way round:
      const c = read_file({ path: f });
      return { path: f, content: c.content };          // ← the mistake. NEVER do this.

    RIGHT — the script looked, and hands you back only the answer:
      const c = read_file({ path: f });
      return { path: f, total_lines: c.total_lines, exports: c.content.match(/^func \w+/gm) };

  Quote a few lines when the lines ARE the answer (a signature, the failing assertion, the line to patch). Never hand back a whole `+"`content`"+`, `+"`output`"+` or `+"`matches`"+` field. If the model of what you need is "everything in the file", you do not need a script — you need to think about what question you are actually asking. Returned bulk is DETECTED and reported back to you as a warning.

THE CONTRACT
  - Your code is the BODY of a function. Use `+"`return`"+` to return a value.
  - Every tool below is a plain SYNCHRONOUS function in scope. Each takes ONE object argument and returns its result object directly. No await, no promises, no callbacks.
  - `+"`console.log(...)`"+` is captured and returned to you.
  - A failing tool call THROWS. Wrap a call in try/catch to handle partial failure and still return something useful; an uncaught throw ends the script (you still get its logs, its calls so far, and the error with its line number).
  - The script has NO other capability: no filesystem, no network, no require/import, no process. It can call the functions below and nothing else.
  - Limits: %s wall clock, %d tool calls, and the returned value is capped.

WHAT YOU GET BACK
  result — your return value (JSON). logs — what you printed. calls — a compact record of each tool call you made (bulky payloads elided; this is also what the trust gate audits your work by). error — set if the script threw. warning — set if you returned the bulk you were supposed to leave behind.

EXAMPLE
  const hits = grep({ pattern: "func Build", path: "internal" });
  const files = [...new Set(hits.matches.map(m => m.path))].slice(0, 5);
  const sizes = {};
  for (const f of files) {
    try {
      sizes[f] = read_file({ path: f }).content.split("\n").length;   // the COUNT, not the content
    } catch (e) {
      sizes[f] = "unreadable: " + e.message;
    }
  }
  return { files, sizes };

APPROVAL
  This whole script is ONE operation. If your deployment guards it, the script itself is reviewed and approved BEFORE any of it runs — so write a program whose effects are plain to read, and expect an approval prompt on the program, not on its individual calls. Tools that would each ask for approval on their own (running commands, pushing) ARE available here: they are covered by the script's own approval.
  The one thing missing from the list below is any tool that has to ask the user a question and wait for the answer — a script cannot pause for a human in the middle of itself. Call those directly, as you always have. Every tool you have remains callable the normal way; this is an addition, not a replacement.`, runCodeTimeout, runCodeMaxCalls)
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

// mapStrings rebuilds v with every string passed through fn — the transforming twin
// of walkStrings. fn returns the replacement and whether it replaced anything; a
// container is only rebuilt if something inside it actually changed, so the common
// case (nothing elided) returns the original value untouched.
//
// goja exports a script's return value as map[string]any / []any / string / float64,
// so those are the shapes that matter; anything else is returned as-is.
func mapStrings(v any, depth int, fn func(string) (string, bool)) any {
	if v == nil || depth > maxWalkDepth {
		return v
	}
	switch t := v.(type) {
	case string:
		if s, changed := fn(t); changed {
			return s
		}
		return t
	case []any:
		out := make([]any, len(t))
		changed := false
		for i, e := range t {
			out[i] = mapStrings(e, depth+1, fn)
			if !reflect.DeepEqual(out[i], e) {
				changed = true
			}
		}
		if !changed {
			return t
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		changed := false
		for k, e := range t {
			out[k] = mapStrings(e, depth+1, fn)
			if !reflect.DeepEqual(out[k], e) {
				changed = true
			}
		}
		if !changed {
			return t
		}
		return out
	}
	return v
}
