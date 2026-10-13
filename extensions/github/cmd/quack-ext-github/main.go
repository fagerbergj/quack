// Command quack-ext-github runs the GitHub App extension as a child of quack (extensions.github.exec).
package main

import (
	_ "github.com/fagerbergj/quack/extensions/github"
	"github.com/fagerbergj/quack/extensions/sdk/extproc"
)

// The extension nil-checks these callbacks, but without one it quietly stops writing context artifacts,
// tracking PR state or refreshing stale clones, so a quack that cannot serve them is refused instead.
func main() {
	extproc.Main("github", extproc.WriteArtifact, extproc.ChatUser, extproc.ArchiveChat, extproc.UpdateChatOrigin,
		extproc.InvalidateSetup, extproc.Classify, extproc.Decide)
}
