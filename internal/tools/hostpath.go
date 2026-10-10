package tools

import (
	"path/filepath"
	"strings"
)

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
