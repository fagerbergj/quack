// Package quack embeds the shipped agents/ and skills/ trees; it must sit at the module root
// because go:embed paths can't climb out with "..".
package quack

import "embed"

// .agents/embedded/dotagents/skills is a tracked snapshot (see its SOURCE.md).
//
//go:embed all:agents all:skills all:.agents/embedded/dotagents/skills all:config/prompts config/rubric.md config/constitution.md
var Embedded embed.FS
