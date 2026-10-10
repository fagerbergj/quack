package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/fagerbergj/quack/internal/eval"
	"github.com/fagerbergj/quack/internal/ledger/bundle"
)

// RunEval replays turns one at a time into a fresh chat on a running server, then scores the chat's own
// recording against the recorded bundle. A worse score is a result (exit 0); non-zero means infrastructure failed.
func RunEval(ctx context.Context, out, errOut io.Writer, base, role, model string, changedAgents, turns []string, recordedScores []bundle.EvalScore, recordedAnswer string, asJSON bool) int {
	c := &Client{BaseURL: strings.TrimRight(base, "/"), HTTP: &http.Client{}}
	chatID, err := c.CreateChat(ctx, "")
	if err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	fmt.Fprintf(errOut, "chat: %s\n", chatID)

	var lastAnswer string
	for i, turn := range turns {
		fmt.Fprintf(errOut, "turn %d/%d: %s\n", i+1, len(turns), preview(turn))
		st := newStreamState()
		fs := newFollowState()
		onEvent := func(ev SSEEvent) error {
			fs.printLine(out, ev)
			st.handle(ev, nil)
			return nil
		}
		if err := c.SendMessage(ctx, chatID, turn, onEvent); err != nil {
			fmt.Fprintln(errOut, err)
			return 1
		}
		res := st.result(chatID)
		if res.Status == StatusFailed {
			fmt.Fprintln(errOut, res.Error)
			return 1
		}
		lastAnswer = res.Answer
	}

	newScores, err := fetchEvalScores(ctx, c, chatID)
	if err != nil {
		fmt.Fprintf(errOut, "warning: could not score the new run (%v) - recording may be disabled\n", err)
	}

	cmp := eval.Build(role, model, changedAgents, recordedScores, newScores, recordedAnswer, lastAnswer)
	if asJSON {
		_ = eval.RenderJSON(out, cmp)
	} else {
		eval.Render(out, cmp)
	}
	return 0
}

// preview truncates a turn's text for the progress line.
func preview(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}

// fetchEvalScores extracts chatID's evaluation.result events from its own recording bundle,
// so the fresh run is scored exactly like the recorded one.
func fetchEvalScores(ctx context.Context, c *Client, chatID string) ([]bundle.EvalScore, error) {
	body, err := c.FetchRecording(ctx, chatID)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp("", "quack-eval-*.zip")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	sess, err := bundle.Load(f.Name())
	if err != nil {
		return nil, err
	}
	return sess.EvaluationResults(), nil
}
