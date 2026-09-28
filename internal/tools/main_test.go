package tools

import "github.com/fagerbergj/quack/internal/workspace"

// Fixtures are local bare repos, so git tests reach them over file:// instead of https.
func init() { workspace.GitProtocol = "file" }
