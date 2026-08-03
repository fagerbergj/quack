package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// RunReplay drives one replayed run against an ALREADY-RUNNING server at
// base (an in-process duck built from a replay-mode config - see cmd/quack's
// `replay` command, which resolves the bundle, synthesizes the config, and
// owns InProcessFromConfig/stop; this function only ever talks HTTP, same as
// every other bulletproof-CLI entry point in this package): create a fresh
// chat, send prompt (the bundle's recorded user turn), and stream the run
// the same way `chat show -f` does, applying the same pause/failure
// exit-code semantics as `-p`/`chat send` (see Report). Returns the process
// exit code.
func RunReplay(ctx context.Context, out, errOut io.Writer, base, prompt string, asJSON bool) int {
	c := &Client{BaseURL: strings.TrimRight(base, "/"), HTTP: &http.Client{}}
	chatID, err := c.CreateChat(ctx, "")
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	fmt.Fprintf(errOut, "chat: %s\n", chatID)

	st := newStreamState()
	fs := newFollowState()
	onEvent := func(ev SSEEvent) error {
		fs.printLine(out, ev)
		st.handle(ev, nil)
		return nil
	}
	if err := c.SendMessage(ctx, chatID, prompt, onEvent); err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	return Report(out, errOut, chatID, st.result(chatID), asJSON)
}
