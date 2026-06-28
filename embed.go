// Package quack is the repository-root package. Its sole job is to host the
// go:embed directives for the shipped agents/ and skills/ trees, which must
// live at the module root (go:embed paths are relative to the source file and
// cannot climb out of the package dir with ".."). internal/bundledir consumes
// the exported embed.FS so the rest of the codebase imports a clean API.
package quack

import "embed"

//go:embed all:agents all:skills
var Embedded embed.FS
