package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/fagerbergj/quack/internal/eval"
	"github.com/fagerbergj/quack/internal/replay"
)

// RunEval drives one eval run against an ALREADY-RUNNING server at base (an
// in-process duck built from a LIVE config with role's model swapped - see
// cmd/quack's `eval` command). It creates a fresh chat and feeds turns into
// it one at a time, turn N+1 only after turn N's run completes, streaming
// progress the same way `chat show -f` does. Once every turn is done it
// fetches the fresh chat's OWN recording, scores it the same way the
// recorded bundle was scored, then prints the comparison.
//
// Exit code is 0 whenever the eval itself completed: a WORSE score is a
// RESULT, not a failure. Non-zero is reserved for an infrastructure failure.
// A fresh recording that can't be fetched/scored degrades to a warning and
// an "n/a" new-side score, not a failed exit.
func RunEval(ctx context.Context, out, errOut io.Writer, base, role, model string, changedAgents, turns []string, recordedScores []replay.EvalScore, recordedAnswer string, asJSON bool) int {
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

// fetchEvalScores downloads chatID's own recording bundle from c (the SAME
// GET .../recording endpoint resolveBundle uses for a recorded chat id) and
// extracts its evaluation.result events, so the fresh run is scored by the
// identical extraction (replay.Session.EvaluationResults) applied to the
// recorded bundle.
func fetchEvalScores(ctx context.Context, c *Client, chatID string) ([]replay.EvalScore, error) {
	body, err := c.FetchRecording(ctx, chatID)
	if err != nil {
		return nil, err
	}
	f, err := os.CreateTemp("", "quack-eval-*.zip")
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(body); err != nil {
		f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	sess, err := replay.Load(f.Name())
	if err != nil {
		return nil, err
	}
	return sess.EvaluationResults(), nil
}
