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

// RunChatList is `quack chat list`: a table of chats (id, title, updated), or raw
// JSON with --json. Empty list points at the next step.
func RunChatList(ctx context.Context, out io.Writer, server string, asJSON bool) error {
	c, err := NewClient(server)
	if err != nil {
		return err
	}
	chats, err := c.ListChats(ctx)
	if err != nil {
		return err
	}
	if asJSON {
		return writeJSON(out, chats)
	}
	if len(chats) == 0 {
		fmt.Fprintln(out, "No chats yet — start one with `quack chat new`.")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTITLE\tUPDATED")
	for _, ch := range chats {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", ch.Id, chatTitle(ch.Title), ch.UpdatedAt.Local().Format("2006-01-02 15:04"))
	}
	return tw.Flush()
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
		if a := strings.TrimSpace(assistantText(t.Output)); a != "" {
			fmt.Fprintf(out, "## Duck\n\n%s\n\n", a)
		}
	}
	return nil
}

// RunChatStop is `quack chat stop <id>`: cancel the active run (no-op if none).
func RunChatStop(ctx context.Context, out io.Writer, server, id string) error {
	c, err := NewClient(server)
	if err != nil {
		return err
	}
	if err := c.CancelRun(ctx, id); err != nil {
		return notFoundAs(err, id)
	}
	fmt.Fprintf(out, "Stopped any active run on chat %s.\n", id)
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

// assistantText concatenates the text of every message output item in a turn,
// skipping DAG and activity items.
func assistantText(items []schema.OutputItem) string {
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
