package tools

import (
	"path/filepath"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

// pathScrub: respells host paths in errors to the model's namespace.
type pathScrub struct {
	inner runnableTool
	b     fsBinding
}

// newPathScrub wraps inner; non-runnable tools pass through.
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

// ProcessRequest packs the wrapper into the request's tool map.
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

// Run is a pass-through except on error, where host paths are respelled.
func (p *pathScrub) Run(ctx agent.Context, args any) (map[string]any, error) {
	res, err := p.inner.Run(ctx, args)
	if err == nil {
		return res, nil
	}
	return res, scrubHostPaths(err, p.b.jail.Root(), p.b.withCwd(ctx).workRoot())
}

// scrubbedError: wraps error with host-path-free message, keeps original in chain.
type scrubbedError struct {
	err error
	msg string
}

func (e *scrubbedError) Error() string { return e.msg }
func (e *scrubbedError) Unwrap() error { return e.err }

// scrubHostPaths: rewrites jail-rooted paths in error messages to the model's namespace.
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

// hostPathEnd: delimiters that terminate a path in error messages.
const hostPathEnd = " \t\r\n\"'`,;)"

// rewriteHostPaths: finds jailRoot-prefixed runs and respells them.
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
		// Trailing ":" or "." belongs to the message, not the path.
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

// modelPath: spells a host path in the model's namespace; paths outside the model's root are elided.
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
