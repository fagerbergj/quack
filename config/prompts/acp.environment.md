{{/* Go template. Placeholders: .Cwd .Git .Branch .Sha .Entries .Truncated .MaxEntries .ReadOnly .Writable .GoModCache */ -}}
<environment_context>
cwd: {{.Cwd}}
{{if .Git}}git: yes (branch {{.Branch}}{{if .Sha}}, HEAD {{.Sha}}{{end}})
{{else}}git: no
{{end}}{{if not .Entries}}entries: (none - empty or unreadable)
{{else if .Truncated}}entries (first {{.MaxEntries}}): {{.Entries}}
{{else}}entries: {{.Entries}}
{{end}}{{if .ReadOnly}}filesystem: read-only (OS-enforced, EACCES on write): {{.Cwd}}
filesystem: writable: {{.Writable}}
reads and execution work anywhere; in-tree writes (npm install, go build artifacts, file edits) fail. To run code against this tree, copy it into a writable path first (`cp -a "$PWD" "$TMPDIR/probe"`) - the copy carries go.mod, so language-level rules like Go's internal/ visibility still resolve.
{{end}}Go's module cache is pre-seeded read-only{{if .GoModCache}} at {{.GoModCache}}{{end}}; GOMODCACHE/GOCACHE are already redirected to a writable copy for you, so `go build`/`go test` need no cache setup of your own.
Toolchains on PATH (go, node, python, gcc, ...) are installed read-only - you cannot add or upgrade one, only use what is there.
For a clean copy of the committed tree with none of your own scratch files mixed in, `git archive HEAD | tar -x -C "$TMPDIR/build"` beats copying the whole working dir.
CI's result, when your task has one, already arrives in the task's envelope - read it there, do not re-run the suite to reproduce it.
</environment_context>
