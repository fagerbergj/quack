package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/fagerbergj/quack/internal/schema"
)

// isGithubChat mirrors frontend/src/lib/github.ts's isGithubChat: GithubUrl is
// the authoritative signal (set by the webhook at dispatch time), the
// "github-" id prefix a fallback for chats persisted before that field existed.
func isGithubChat(c schema.ChatSummary) bool {
	return c.GithubUrl != nil || strings.HasPrefix(c.Id, "github-")
}

var githubURLRE = regexp.MustCompile(`/(issues|pull)/(\d+)`)

type githubRef struct {
	Repo   string
	Kind   string // "issue" or "pr"
	Number int
}

// parseGithubRef mirrors frontend/src/lib/github.ts's parseGithubRef: the
// issue/PR kind + number come off GithubUrl's path shape, the repo off
// GithubRepo (falling back to the URL's owner/repo segment).
func parseGithubRef(c schema.ChatSummary) (githubRef, bool) {
	if c.GithubUrl == nil || *c.GithubUrl == "" {
		return githubRef{}, false
	}
	m := githubURLRE.FindStringSubmatch(*c.GithubUrl)
	if m == nil {
		return githubRef{}, false
	}
	kind := "issue"
	if m[1] == "pull" {
		kind = "pr"
	}
	repo := ""
	if c.GithubRepo != nil {
		repo = *c.GithubRepo
	} else if parts := strings.SplitN(strings.TrimPrefix(*c.GithubUrl, "https://github.com/"), "/", 3); len(parts) >= 2 {
		repo = parts[0] + "/" + parts[1]
	}
	n, _ := strconv.Atoi(m[2])
	return githubRef{Repo: repo, Kind: kind, Number: n}, true
}

// githubRefLabel renders "Issue #249" / "PR #257", or "-" when c isn't a
// GitHub-originated chat with a parseable ref.
func githubRefLabel(c schema.ChatSummary) string {
	ref, ok := parseGithubRef(c)
	if !ok {
		return "-"
	}
	if ref.Kind == "pr" {
		return fmt.Sprintf("PR #%d", ref.Number)
	}
	return fmt.Sprintf("Issue #%d", ref.Number)
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

// chatListFilters bundles the four `chat list` narrowing flags. Each empty
// field imposes no constraint; validate() rejects unrecognised values before
// any of them reach the keep-checks below (mirrors frontend/src/lib/chatFilters.ts's
// matchesFacets: every active facet must match, no ordering dependency).
type chatListFilters struct {
	origin string // "", "all", "github", "direct"
	status string // "", or a ChatStatus value
	repo   string // "", or an exact owner/repo match
	kind   string // "", "issue", "pr"
}

// NewChatListFilters builds a chatListFilters from the `chat list` flag
// values (origin filter, status, repo, github ref type).
func NewChatListFilters(origin, status, repo, kind string) chatListFilters {
	return chatListFilters{origin: origin, status: status, repo: repo, kind: kind}
}

func (f chatListFilters) validate() error {
	if f.origin != "" && f.origin != "all" && f.origin != "github" && f.origin != "direct" {
		return fmt.Errorf("--filter must be one of all, github, direct (got %q)", f.origin)
	}
	switch f.status {
	case "", "idle", "queued", "running", "needs_input", "failed":
	default:
		return fmt.Errorf("--status must be one of idle, queued, running, needs_input, failed (got %q)", f.status)
	}
	switch f.kind {
	case "", "issue", "pr":
	default:
		return fmt.Errorf("--type must be one of issue, pr (got %q)", f.kind)
	}
	return nil
}

func (f chatListFilters) keep(c schema.ChatSummary) bool {
	if !originFilterKeep(c, f.origin) {
		return false
	}
	if f.status != "" && string(c.Status) != f.status {
		return false
	}
	if f.repo != "" {
		ref, ok := parseGithubRef(c)
		if !ok || ref.Repo != f.repo {
			return false
		}
	}
	if f.kind != "" {
		ref, ok := parseGithubRef(c)
		if !ok || ref.Kind != f.kind {
			return false
		}
	}
	return true
}

// RunChatList is `quack chat list`: a table of chats (id, title, status,
// origin, ref, updated), or raw JSON with --json. STATUS is one of the five
// ChatStatus values (queued/running/needs_input/failed/idle) so the row is
// grep-able (`grep needs_input`); the pending question itself is `chat
// show`/--json's job - this table stays narrow. filters narrows by origin,
// status, github repo, and issue/PR type - a chat must pass every active one
// (mirrors the web sidebar's facet filtering). Empty list points at the next step.
func RunChatList(ctx context.Context, out io.Writer, server string, asJSON bool, filters chatListFilters) error {
	if err := filters.validate(); err != nil {
		return err
	}
	c, err := NewClient(ctx, server)
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
		if filters.keep(ch) {
			kept = append(kept, ch)
		}
	}
	chats = kept
	if asJSON {
		return writeJSON(out, chats)
	}
	if len(chats) == 0 {
		if hadAny {
			fmt.Fprintln(out, "No chats match the given filters.")
			return nil
		}
		fmt.Fprintln(out, "No chats yet - start one with `quack chat new`.")
		return nil
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTITLE\tSTATUS\tORIGIN\tREF\tUPDATED")
	for _, ch := range chats {
		origin := "direct"
		if isGithubChat(ch) {
			origin = "github"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", ch.Id, chatTitle(ch.Title), ch.Status, origin, githubRefLabel(ch), ch.UpdatedAt.Local().Format("2006-01-02 15:04"))
	}
	return tw.Flush()
}

// RunChatNew is `quack chat new`: create a chat and print its id to stdout -
// create-only, no TUI, no first-message send (that's `chat send`/`-p`'s job,
// one send path).
func RunChatNew(ctx context.Context, out io.Writer, server string) error {
	c, err := NewClient(ctx, server)
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
// message text (DAG/activity items are omitted - use --json for the full record).
func RunChatExport(ctx context.Context, out io.Writer, server, id string, asJSON bool) error {
	c, err := NewClient(ctx, server)
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
	c, err := NewClient(ctx, server)
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
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	if err := c.CancelNode(ctx, chatID, nodeID); err != nil {
		return notFoundAs(err, chatID)
	}
	fmt.Fprintf(out, "Stopped node %s (chat %s); the rest of the run continues.\n", nodeID, chatID)
	return nil
}

// RunNodePause is `quack chat node pause <chat-id> <node-id>`: suspend one
// RUNNING node at its next turn boundary, keeping its accumulated work.
func RunNodePause(ctx context.Context, out io.Writer, server, chatID, nodeID string) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	if err := c.PauseNode(ctx, chatID, nodeID); err != nil {
		return notFoundAs(err, chatID)
	}
	fmt.Fprintf(out, "Paused node %s (chat %s) - resume it with `quack chat node resume %s %s`.\n", nodeID, chatID, chatID, nodeID)
	return nil
}

// RunNodeResume is `quack chat node resume <chat-id> <node-id>`: resume a
// PAUSED node - a fresh re-run (like retry), reusing the rest of the plan's
// stored outputs. Watch it with `chat show -f`.
func RunNodeResume(ctx context.Context, out io.Writer, server, chatID, nodeID string) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	if err := c.ResumeNode(ctx, chatID, nodeID); err != nil {
		return notFoundAs(err, chatID)
	}
	fmt.Fprintf(out, "Resuming node %s (chat %s) - watch it with `quack chat show %s -f`.\n", nodeID, chatID, chatID)
	return nil
}

// RunNodeQueue is `quack chat node queue <chat-id> <node-id> <message>`:
// append a message to a RUNNING node's queue, delivered at its next turn
// boundary (never mid-turn) - replaces the old interrupt-based steer.
func RunNodeQueue(ctx context.Context, out io.Writer, server, chatID, nodeID, message string) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	m, err := c.QueueNodeMessage(ctx, chatID, nodeID, message)
	if err != nil {
		return notFoundAs(err, chatID)
	}
	fmt.Fprintf(out, "Queued message %s for node %s (chat %s) - delivered at its next turn boundary.\n", m.Id, nodeID, chatID)
	return nil
}

// RunNodeQueueEdit is `quack chat node queue-edit <chat-id> <node-id> <message-id> <text>`:
// rewrite a not-yet-delivered queued message.
func RunNodeQueueEdit(ctx context.Context, out io.Writer, server, chatID, nodeID, messageID, text string) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	if err := c.EditQueuedMessage(ctx, chatID, nodeID, messageID, text); err != nil {
		return notFoundAs(err, chatID)
	}
	fmt.Fprintf(out, "Edited queued message %s for node %s (chat %s).\n", messageID, nodeID, chatID)
	return nil
}

// RunNodeQueueRemove is `quack chat node queue-remove <chat-id> <node-id> <message-id>`:
// drop a not-yet-delivered queued message.
func RunNodeQueueRemove(ctx context.Context, out io.Writer, server, chatID, nodeID, messageID string) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	if err := c.RemoveQueuedMessage(ctx, chatID, nodeID, messageID); err != nil {
		return notFoundAs(err, chatID)
	}
	fmt.Fprintf(out, "Removed queued message %s for node %s (chat %s).\n", messageID, nodeID, chatID)
	return nil
}

// RunNodeEditTask is `quack chat node edit <chat-id> <node-id> <task>`:
// replace a not-yet-started node's prompt. Errors (409) once the node has
// started - its prompt is then immutable.
func RunNodeEditTask(ctx context.Context, out io.Writer, server, chatID, nodeID, task string) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	if err := c.EditNodeTask(ctx, chatID, nodeID, task); err != nil {
		return notFoundAs(err, chatID)
	}
	fmt.Fprintf(out, "Edited node %s's prompt (chat %s).\n", nodeID, chatID)
	return nil
}

// RunNodeRetry is `quack chat node retry <chat-id> <node-id> [--guidance]`:
// re-queue a finished node (done/failed/cancelled); it and everything
// downstream re-run, reusing the stored outputs of all other nodes.
func RunNodeRetry(ctx context.Context, out io.Writer, server, chatID, nodeID, guidance string) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	if err := c.RetryNode(ctx, chatID, nodeID, guidance); err != nil {
		return notFoundAs(err, chatID)
	}
	fmt.Fprintf(out, "Retrying node %s (chat %s) - watch it with `quack chat show %s -f`.\n", nodeID, chatID, chatID)
	return nil
}

// RunChatDelete is `quack chat delete <id>`. Deletion is irreversible, so it
// confirms first unless yes is set (the --yes flag, or a non-interactive stdin).
func RunChatDelete(ctx context.Context, out io.Writer, in io.Reader, server, id string, yes bool) error {
	c, err := NewClient(ctx, server)
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
			// OutputTextPart and ReasoningPart share the same {text, type}
			// shape, so AsOutputTextPart() unmarshals a reasoning part
			// without error too - check the discriminator first, or the
			// orchestrator's raw chain-of-thought leaks in as if it were
			// the answer (#419).
			disc, err := part.Discriminator()
			if err != nil || disc != string(schema.OutputText) {
				continue
			}
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
		// Fscanln errors on a blank line / EOF - treat as "no", not a failure.
		return false, nil
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}
