// Package quack is the repository-root package. Its sole job is to host the
// go:embed directives for the shipped agents/ and skills/ trees, which must
// live at the module root (go:embed paths are relative to the source file and
// cannot climb out of the package dir with ".."). internal/bundledir consumes
// the exported embed.FS so the rest of the codebase imports a clean API.
package quack

import "embed"

// The dotagents skills are vendored in-tree (.agents/vendor/plugins.yaml
// records upstream), so a plain clone builds - no fetch step, no network.
//
//go:embed all:agents all:skills all:.agents/vendor/dotagents/skills
var Embedded embed.FS
