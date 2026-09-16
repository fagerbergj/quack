// Package quack is the repository-root package hosting the go:embed directives for the
// shipped agents/ and skills/ trees (must stay at the module root: go:embed paths can't
// climb out with ".."). internal/bundledir consumes the exported embed.FS as a clean API.
package quack

import "embed"

// .agents/embedded/dotagents/skills is a tracked snapshot (see its SOURCE.md); config/*.md and config/prompts are what internal/artifactsrc resolves a name to.
//
//go:embed all:agents all:skills all:.agents/embedded/dotagents/skills all:config/prompts config/rubric.md config/constitution.md
var Embedded embed.FS
