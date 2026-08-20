// Package quack is the repository-root package. Its sole job is to host the
// go:embed directives for the shipped agents/ and skills/ trees, which must
// live at the module root (go:embed paths are relative to the source file and
// cannot climb out of the package dir with ".."). internal/bundledir consumes
// the exported embed.FS so the rest of the codebase imports a clean API.
package quack

import "embed"

// .agents/vendor/dotagents is NOT in git: .agents/vendor/plugins.yaml pins it
// and `make plugins` fetches it. On a fresh clone, run that first or this
// embed fails with "pattern all:.agents/vendor/dotagents/skills: no matching
// files found" (make build/test do it for you).
//
//go:embed all:agents all:skills all:.agents/vendor/dotagents/skills
var Embedded embed.FS
