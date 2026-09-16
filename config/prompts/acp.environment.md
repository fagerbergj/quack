{{/* Go template. Placeholders: .Cwd .Git .Branch .Sha .Entries .Truncated .MaxEntries .ReadOnly .Writable */ -}}
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
{{end}}</environment_context>
