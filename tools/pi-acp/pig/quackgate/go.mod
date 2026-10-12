module github.com/fagerbergj/quack/tools/pi-acp/pig/quackgate

// PIG's own go line: a newer one makes the Piglet build demand a go.mod update and fail.
go 1.26.0

// PIG swaps in its own version-matched SDK when it fuses the gate into a Piglet.
require github.com/MichaelKinsy/PiG/extensions/sdk v0.4.1
