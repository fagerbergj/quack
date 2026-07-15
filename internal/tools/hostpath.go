package tools

import (
	"path/filepath"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

// pathScrub keeps the one-namespace rule (see cwd.go) in ERRORS too: os/git hand
// back RESOLVED host paths (*fs.PathError, git stderr) that every tool wraps with
// %w, so the model would be answered in a namespace it cannot type or correct.
// Applied ONCE, at registry.Build's single wrap point, around every tool — a tool
// added tomorrow is scrubbed without its author knowing this file exists
// (TestEveryBuiltToolIsPathScrubbed enforces it). It REWRITES rather than
// redacts: the same location respelled in the model's namespace, so the error
// stays actionable.
type pathScrub struct {
	inner runnableTool
	// b carries (userID, jail): enough to compute, per call, where the MODEL's
	// root is (b.withCwd(ctx).workRoot() — the node's invisible root under this
	// chat's scope) and therefore how to spell a resolved path back in the
	// model's namespace.
	b fsBinding
}

// newPathScrub wraps inner. A tool that is not runnable is returned untouched —
// there is no Run to scrub, and failing the build over it would be a worse trade
// than passing it through.
func newPathScrub(inner tool.Tool, b fsBinding) tool.Tool {
	rt, ok := inner.(runnableTool)
	if !ok {
		return inner
	}
	return &pathScrub{inner: rt, b: b}
}

func (p *pathScrub) Name() string        { return p.inner.Name() }
func (p *pathScrub) Description() string { return p.inner.Description() }
func (p *pathScrub) IsLongRunning() bool { return p.inner.IsLongRunning() }

func (p *pathScrub) Declaration() *genai.FunctionDeclaration { return p.inner.Declaration() }

// ProcessRequest packs the WRAPPER into the request's tool map, for the same
// reason cancelGuard and guardedTool do: delegating would register the inner tool
// under the name and bypass this Run entirely.
func (p *pathScrub) ProcessRequest(ctx agent.Context, req *model.LLMRequest) error {
	if err := p.inner.ProcessRequest(ctx, req); err != nil {
		return err
	}
	if req.Tools != nil {
		if _, ok := req.Tools[p.Name()]; ok {
			req.Tools[p.Name()] = p
		}
	}
	return nil
}

// Run is a pass-through except on the error path, where every host path in the
// message is respelled in the model's namespace.
func (p *pathScrub) Run(ctx agent.Context, args any) (map[string]any, error) {
	res, err := p.inner.Run(ctx, args)
	if err == nil {
		return res, nil
	}
	return res, scrubHostPaths(err, p.b.jail.Root(), p.b.withCwd(ctx).workRoot())
}

// scrubbedError keeps the original error in the chain (errors.Is/As still work,
// and the cancel guard's own message-matching is untouched) while presenting the
// model a message with no host path in it.
type scrubbedError struct {
	err error
	msg string
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.err }

// scrubHostPaths rewrites every path under jailRoot in err's message into the
// model's namespace. err is returned untouched when it names no host path at all,
// which is the common case (an escape rejection, a bad argument, git's own
// complaint about a ref).
func scrubHostPaths(err error, jailRoot, modelRoot string) error {
	if err == nil || jailRoot == "" {
		return err
	}
	msg := err.Error()
	out := rewriteHostPaths(msg, jailRoot, modelRoot)
	if out == msg {
		return err
	}
	return &scrubbedError{err: err, msg: out}
}

// hostPathEnd are the bytes that cannot be inside a path we care about, so they
// end one: whitespace and the quoting an error message wraps a path in.
const hostPathEnd = " \t\r\n\"'`,;)"

// rewriteHostPaths finds every jailRoot-prefixed run in s and respells it. The
// scan is deliberately dumb (no regexp): a path starts where the root does and
// ends at the first delimiter — plus a trailing ":" or "." peeled off, because
// os's own "stat <path>: no such file" and git's "<path>: not a directory" put one
// there.
func rewriteHostPaths(s, jailRoot, modelRoot string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, jailRoot)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		rest := s[i:]
		end := strings.IndexAny(rest, hostPathEnd)
		if end < 0 {
			end = len(rest)
		}
		token := rest[:end]
		// "stat /root/a/b.go: no such file" — the colon belongs to the message.
		trailing := ""
		for len(token) > 0 && strings.ContainsRune(":.", rune(token[len(token)-1])) {
			trailing = string(token[len(token)-1]) + trailing
			token = token[:len(token)-1]
		}
		b.WriteString(modelPath(token, modelRoot))
		b.WriteString(trailing)
		s = rest[end:]
	}
}

// modelPath spells a resolved host path the way the MODEL spells it: absolute
// within its own root ("/internal/tools/registry.go" — the spelling jailPath and
// displayCwd already speak, so it can be fed straight back into any tool).
//
// A path under the jail but OUTSIDE the model's own root (another node's tree,
// another chat's) is not respellable in the model's namespace at all — there is
// no such place from where it stands — so it is elided rather than translated.
func modelPath(real, modelRoot string) string {
	if modelRoot == "" {
		return "(a path outside your workspace)"
	}
	rel, err := filepath.Rel(modelRoot, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "(a path outside your workspace)"
	}
	if rel == "." {
		return "/"
	}
	return "/" + filepath.ToSlash(rel)
}
