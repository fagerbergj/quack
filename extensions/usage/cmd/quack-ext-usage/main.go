// Command quack-ext-usage runs the usage extension as a child of quack (extensions.usage.exec).
package main

import (
	"github.com/fagerbergj/quack/extensions/sdk/extproc"
	_ "github.com/fagerbergj/quack/extensions/usage"
)

func main() { extproc.Main("usage") }
