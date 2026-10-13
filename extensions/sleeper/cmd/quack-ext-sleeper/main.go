// Command quack-ext-sleeper runs the sleeper extension as a child of quack (extensions.sleeper.exec).
package main

import (
	"github.com/fagerbergj/quack/extensions/sdk/extproc"
	_ "github.com/fagerbergj/quack/extensions/sleeper"
)

func main() { extproc.Main("sleeper") }
