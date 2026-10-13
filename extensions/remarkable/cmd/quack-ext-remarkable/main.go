// Command quack-ext-remarkable runs the remarkable extension as a child of quack (extensions.remarkable.exec).
package main

import (
	_ "github.com/fagerbergj/quack/extensions/remarkable"
	"github.com/fagerbergj/quack/extensions/sdk/extproc"
)

func main() { extproc.Main("remarkable") }
