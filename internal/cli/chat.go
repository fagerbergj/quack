package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/fagerbergj/quack/internal/schema"
)

// isGithubChat mirrors frontend/src/pages/GitHubSessions.tsx's isGithubChat:
// GithubUrl is the authoritative signal (set by the webhook at dispatch
// time), the "github-" id prefix a fallback for chats persisted before that
// field existed.
func isGithubChat(c schema.ChatSummary) bool {
	return c.GithubUrl != nil || strings.HasPrefix(c.Id, "github-")
}

// originFilterKeep reports whether c passes the --filter value ("all",
// "github", "direct"); an unrecognised value keeps everything (validated by
// the caller before this is ever reached).
func originFilterKeep(c schema.ChatSummary, filter string) bool {
	switch filter {
	case "github":
		return isGithubChat(c)
	case "direct":
		return !isGithubChat(c)
	default:
		return true
	}
}

// RunChatList is `quack chat list`: a table of chats (id, title, status,
// origin, updated), or raw JSON with --json. STATUS is one of the four
// ChatStatus values (running/needs_input/failed/idle) so the row is
// grep-able (`grep needs_input`); the pending question itself is `chat
// show`/--json's job — this table stays narrow. filter narrows to
// "github"/"direct" origin chats ("all" or "" keeps everything). Empty list
// points at the next step.
func RunChatList(ctx context.Context, out io.Writer, server string, asJSON bool, filter string) error {
	if filter != "" && filter != "all" && filter != "github" && filter != "direct" {
		return fmt.Errorf("--filter must be one of all, github, direct (got %q)", filter)
	}
	c, err := NewClient(server)
	if err != nil {
		return err
	}
	chats, err := c.ListChats(ctx)
	if err != nil {
		return err
	}
	hadAny := len(chats) > 0
	kept := chats[:0:0]
	for _, ch := range chats {
		if originFilterKeep(ch, filter) {
			kept = append(kept, ch)
		}
	}
	chats = kept
	if asJSON {
		return writeJSON(out, chats)
	}
	if len(chats) == 0 {
		if hadAny {
			fmt.Fprintf(out, "No %s chats.\n", filter)
			return nil
		}
		fmt.Fprintln(out, "No chats yet — start one with `quack chat new`.")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTITLE\tSTATUS\tORIGIN\tUPDATED")
	for _, ch := range chats {
		origin := "direct"
		if isGithubChat(ch) {
			origin = "github"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", ch.Id, chatTitle(ch.Title), ch.Status, origin, ch.UpdatedAt.Local().Format("2006-01-02 15:04"))
	}
	return tw.Flush()
}

// RunChatNew is `quack chat new`: create a chat and print its id to stdout —
// create-only, no TUI, no first-message send (that's `chat send`/`-p`'s job,
// one send path).
func RunChatNew(ctx context.Context, out io.Writer, server string) error {
	c, err := NewClient(server)
	if err != nil {
		return err
	}
	id, err := c.CreateChat(ctx, "")
	if err != nil {
		return err
	}
	fmt.Fprintln(out, id)
	return nil
}

// RunChatExport is `quack chat export <id>`: a readable transcript, or raw JSON
// with --json. The transcript pairs each turn's user input with the assistant's
// message text (DAG/activity items are omitted — use --json for the full record).
func RunChatExport(ctx context.Context, out io.Writer, server, id string, asJSON bool) error {
	c, err := NewClient(server)
	if err != nil {
		return err
	}
	detail, err := c.GetChat(ctx, id)
	if err != nil {
		return notFoundAs(err, id)
	}
	if asJSON {
		return writeJSON(out, detail)
	}
	title := chatTitle(detail.Title)
	fmt.Fprintf(out, "# %s\n\n", title)
	for _, t := range detail.Turns {
		fmt.Fprintf(out, "## You\n\n%s\n\n", strings.TrimSpace(t.Input.Content))
		if a := strings.TrimSpace(AssistantText(t.Output)); a != "" {
			fmt.Fprintf(out, "## Duck\n\n%s\n\n", a)
		}
	}
	return nil
}

// RunChatStop is `quack chat stop <id>`: cancel the active run (no-op if none).
// Cancelling by response id is the server's only cancel path now, so this
// looks up the chat's latest turn (the in-progress run, if any) first.
func RunChatStop(ctx context.Context, out io.Writer, server, id string) error {
	c, err := NewClient(server)
	if err != nil {
		return err
	}
	detail, err := c.GetChat(ctx, id)
	if err != nil {
		return notFoundAs(err, id)
	}
	if len(detail.Turns) == 0 {
		fmt.Fprintf(out, "No active run on chat %s.\n", id)
		return nil
	}
	responseID := detail.Turns[len(detail.Turns)-1].Id
	if err := c.CancelRun(ctx, id, responseID); err != nil {
		if errors.Is(err, ErrNotFound) {
			fmt.Fprintf(out, "No active run on chat %s.\n", id)
			return nil
		}
		return err
	}
	fmt.Fprintf(out, "Stopped any active run on chat %s.\n", id)
	return nil
}

// RunNodeStop is `quack chat node stop <chat-id> <node-id>`: cancel one running
// node; the rest of the run continues. No-op if no such node is active.
func RunNodeStop(ctx context.Context, out io.Writer, server, chatID, nodeID string) error {
	c, err := NewClient(server)
	if err != nil {
		return err
	}
	if err := c.CancelNode(ctx, chatID, nodeID); err != nil {
		return notFoundAs(err, chatID)
	}
	fmt.Fprintf(out, "Stopped node %s (chat %s); the rest of the run continues.\n", nodeID, chatID)
	return nil
}

// RunNodeSteer is `quack chat node steer <chat-id> <node-id> <guidance>`:
// interrupt one RUNNING node and re-run it against its same session with the
// guidance folded in. The re-run streams over the chat's existing SSE
// connection — watch it with `chat show -f`.
func RunNodeSteer(ctx context.Context, out io.Writer, server, chatID, nodeID, guidance string) error {
	c, err := NewClient(server)
	if err != nil {
		return err
	}
	if err := c.SteerNode(ctx, chatID, nodeID, guidance); err != nil {
		return notFoundAs(err, chatID)
	}
	fmt.Fprintf(out, "Steered node %s (chat %s) — watch it with `quack chat show %s -f`.\n", nodeID, chatID, chatID)
	return nil
}

// RunNodeRetry is `quack chat node retry <chat-id> <node-id> [--guidance]`:
// re-queue a finished node (done/failed/cancelled); it and everything
// downstream re-run, reusing the stored outputs of all other nodes.
func RunNodeRetry(ctx context.Context, out io.Writer, server, chatID, nodeID, guidance string) error {
	c, err := NewClient(server)
	if err != nil {
		return err
	}
	if err := c.RetryNode(ctx, chatID, nodeID, guidance); err != nil {
		return notFoundAs(err, chatID)
	}
	fmt.Fprintf(out, "Retrying node %s (chat %s) — watch it with `quack chat show %s -f`.\n", nodeID, chatID, chatID)
	return nil
}

// RunChatDelete is `quack chat delete <id>`. Deletion is irreversible, so it
// confirms first unless yes is set (the --yes flag, or a non-interactive stdin).
func RunChatDelete(ctx context.Context, out io.Writer, in io.Reader, server, id string, yes bool) error {
	c, err := NewClient(server)
	if err != nil {
		return err
	}
	if !yes {
		ok, err := confirm(out, in, fmt.Sprintf("Delete chat %s? This cannot be undone.", id))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(out, "Cancelled.")
			return nil
		}
	}
	if err := c.DeleteChat(ctx, id); err != nil {
		return notFoundAs(err, id)
	}
	fmt.Fprintf(out, "Deleted chat %s.\n", id)
	return nil
}

// chatTitle renders a chat's title, falling back to a placeholder.
func chatTitle(t *string) string {
	if t != nil && strings.TrimSpace(*t) != "" {
		return *t
	}
	return "(untitled)"
}

// AssistantText concatenates the text of every message output item in a turn,
// skipping DAG and activity items. Shared by export and chat show.
func AssistantText(items []schema.OutputItem) string {
	var sb strings.Builder
	for _, it := range items {
		m, err := it.AsMessageOutputItem()
		if err != nil || string(m.Type) != "message" {
			continue
		}
		for _, part := range m.Content {
			if tp, err := part.AsOutputTextPart(); err == nil {
				sb.WriteString(tp.Text)
			}
		}
	}
	return sb.String()
}

// notFoundAs turns the client's ErrNotFound into a chat-specific message.
func notFoundAs(err error, id string) error {
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("chat %s not found", id)
	}
	return err
}

func writeJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// confirm asks a yes/no question on out and reads a line from in. Defaults to no
// on a blank line or EOF (a closed/empty stdin → safe default, not a hang).
func confirm(out io.Writer, in io.Reader, prompt string) (bool, error) {
	fmt.Fprintf(out, "%s [y/N] ", prompt)
	var answer string
	if _, err := fmt.Fscanln(in, &answer); err != nil {
		// Fscanln errors on a blank line / EOF — treat as "no", not a failure.
		return false, nil
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}
