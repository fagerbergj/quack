package cli

import (
	"context"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
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

// chatListFilters bundles the `chat list` narrowing flags. Each empty
// field imposes no constraint; validate() rejects unrecognised values before
// any of them reach the keep-checks below (mirrors frontend/src/lib/chatFilters.ts's matchesFacets: every active facet must match, no ordering dependency). archived is the odd one out: it picks the server's active/archived scope (the status= query param) rather than a client-side keep-check.
type chatListFilters struct {
	origin   string // "", "all", "github", "direct"
	status   string // "", or a ChatStatus value
	repo     string // "", or an exact owner/repo match
	kind     string // "", "issue", "pr"
	archived string // "", "exclude", "include", "only"
}

// NewChatListFilters builds a chatListFilters from the `chat list` flag
// values (origin filter, status, repo, github ref type, archive scope).
func NewChatListFilters(origin, status, repo, kind, archived string) chatListFilters {
	return chatListFilters{origin: origin, status: status, repo: repo, kind: kind, archived: archived}
}

func (f chatListFilters) validate() error {
	if f.origin != "" && f.origin != "all" && f.origin != "github" && f.origin != "direct" {
		return fmt.Errorf("--filter must be one of all, github, direct (got %q)", f.origin)
	}
	switch f.status {
	case "", "idle", "running", "needs_input", "failed":
	default:
		return fmt.Errorf("--status must be one of idle, running, needs_input, failed (got %q)", f.status)
	}
	switch f.kind {
	case "", "issue", "pr":
	default:
		return fmt.Errorf("--type must be one of issue, pr (got %q)", f.kind)
	}
	switch f.archived {
	case "", "exclude", "include", "only":
	default:
		return fmt.Errorf("--archived must be one of exclude, include, only (got %q)", f.archived)
	}
	return nil
}

// serverStatuses maps archived to status= query values; "" passes nil so a
// server default change (today "active") doesn't need mirroring here.
func (f chatListFilters) serverStatuses() []string {
	switch f.archived {
	case "include":
		return []string{"active", "archived"}
	case "only":
		return []string{"archived"}
	default:
		return nil
	}
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
// origin, ref, updated), or raw JSON with --json. STATUS is one of the four
// ChatStatus values (running/needs_input/failed/idle) so the row is grep-able (`grep needs_input`); the pending question itself is `chat show`/--json's job - this table stays narrow. filters narrows by origin, status, github repo, and issue/PR type - a chat must pass every active one (mirrors the web sidebar's facet filtering). Empty list points at the next step.
func RunChatList(ctx context.Context, out io.Writer, server string, asJSON bool, filters chatListFilters) error {
	if err := filters.validate(); err != nil {
		return err
	}
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	chats, err := c.ListChats(ctx, filters.serverStatuses())
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
		return WriteJSON(out, chats)
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
		return WriteJSON(out, detail)
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

// nodeActionResult is the shared --json shape for chat stop/delete and the
// chat node verbs - one struct so every action command's output looks alike.
type nodeActionResult struct {
	ChatID    string `json:"chat_id"`
	NodeID    string `json:"node_id,omitempty"`
	MessageID string `json:"message_id,omitempty"`
	Action    string `json:"action"`
	Message   string `json:"message"`
}

// reportAction writes r as JSON with --json, else just its message line -
// the one place these action commands choose their output shape.
func reportAction(out io.Writer, asJSON bool, r nodeActionResult) error {
	if asJSON {
		return WriteJSON(out, r)
	}
	fmt.Fprintln(out, r.Message)
	return nil
}

// RunChatStop is `quack chat stop <id>`: cancel the active run (no-op if none).
// Cancelling by response id is the server's only cancel path now, so this
// looks up the chat's latest turn (the in-progress run, if any) first.
func RunChatStop(ctx context.Context, out io.Writer, server, id string, asJSON bool) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	noneActive := nodeActionResult{ChatID: id, Action: "none", Message: fmt.Sprintf("No active run on chat %s.", id)}
	detail, err := c.GetChat(ctx, id)
	if err != nil {
		return notFoundAs(err, id)
	}
	if len(detail.Turns) == 0 {
		return reportAction(out, asJSON, noneActive)
	}
	responseID := detail.Turns[len(detail.Turns)-1].Id
	if err := c.CancelRun(ctx, id, responseID); err != nil {
		if errors.Is(err, ErrNotFound) {
			return reportAction(out, asJSON, noneActive)
		}
		return err
	}
	return reportAction(out, asJSON, nodeActionResult{ChatID: id, Action: "stopped", Message: fmt.Sprintf("Stopped any active run on chat %s.", id)})
}

// RunChatRename is `quack chat rename <id> <title>`: PATCH the chat's title.
func RunChatRename(ctx context.Context, out io.Writer, server, id, title string) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	if _, err := c.UpdateChat(ctx, id, &title, nil); err != nil {
		return notFoundAs(err, id)
	}
	fmt.Fprintf(out, "Renamed chat %s to %q.\n", id, title)
	return nil
}

// RunChatArchive backs both `chat archive` and `chat unarchive`.
func RunChatArchive(ctx context.Context, out io.Writer, server, id string, archived bool) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	if _, err := c.UpdateChat(ctx, id, nil, &archived); err != nil {
		return notFoundAs(err, id)
	}
	verb := "Archived"
	if !archived {
		verb = "Unarchived"
	}
	fmt.Fprintf(out, "%s chat %s.\n", verb, id)
	return nil
}

// RunNodeStop is `quack chat node stop <chat-id> <node-id>`: cancel one running
// node; the rest of the run continues. No-op if no such node is active.
func RunNodeStop(ctx context.Context, out io.Writer, server, chatID, nodeID string, asJSON bool) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	if err := c.CancelNode(ctx, chatID, nodeID); err != nil {
		return nodeErrAs(err, chatID)
	}
	return reportAction(out, asJSON, nodeActionResult{ChatID: chatID, NodeID: nodeID, Action: "stop",
		Message: fmt.Sprintf("Stopping node %s (chat %s); an in-flight round is being aborted, the rest of the run continues.", nodeID, chatID)})
}

// RunNodePause is `quack chat node pause <chat-id> <node-id>`: suspend one
// RUNNING node at its next turn boundary, keeping its accumulated work.
func RunNodePause(ctx context.Context, out io.Writer, server, chatID, nodeID string, asJSON bool) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	if err := c.PauseNode(ctx, chatID, nodeID); err != nil {
		return nodeErrAs(err, chatID)
	}
	return reportAction(out, asJSON, nodeActionResult{ChatID: chatID, NodeID: nodeID, Action: "pause",
		Message: fmt.Sprintf("Pausing node %s (chat %s) at its next turn boundary - resume it with `quack chat node resume %s %s`.", nodeID, chatID, chatID, nodeID)})
}

// RunNodeResume is `quack chat node resume <chat-id> <node-id>`: resume a
// PAUSED node - a fresh re-run (like retry), reusing the rest of the plan's
// stored outputs. Watch it with `chat show -f`.
func RunNodeResume(ctx context.Context, out io.Writer, server, chatID, nodeID string, asJSON bool) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	if err := c.ResumeNode(ctx, chatID, nodeID); err != nil {
		return nodeErrAs(err, chatID)
	}
	return reportAction(out, asJSON, nodeActionResult{ChatID: chatID, NodeID: nodeID, Action: "resume",
		Message: fmt.Sprintf("Resuming node %s (chat %s) - watch it with `quack chat show %s -f`.", nodeID, chatID, chatID)})
}

// RunNodeQueue is `quack chat node queue <chat-id> <node-id> <message>`:
// append a message to a RUNNING node's queue, delivered at its next turn
// boundary (never mid-turn) - replaces the old interrupt-based steer.
func RunNodeQueue(ctx context.Context, out io.Writer, server, chatID, nodeID, message string, asJSON bool) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	m, err := c.QueueNodeMessage(ctx, chatID, nodeID, message)
	if err != nil {
		return nodeErrAs(err, chatID)
	}
	return reportAction(out, asJSON, nodeActionResult{ChatID: chatID, NodeID: nodeID, MessageID: m.Id, Action: "queue",
		Message: fmt.Sprintf("Queued message %s for node %s (chat %s) - delivered at its next turn boundary.", m.Id, nodeID, chatID)})
}

// RunNodeQueueEdit is `quack chat node queue-edit <chat-id> <node-id> <message-id> <text>`:
// rewrite a not-yet-delivered queued message.
func RunNodeQueueEdit(ctx context.Context, out io.Writer, server, chatID, nodeID, messageID, text string, asJSON bool) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	if err := c.EditQueuedMessage(ctx, chatID, nodeID, messageID, text); err != nil {
		return nodeErrAs(err, chatID)
	}
	return reportAction(out, asJSON, nodeActionResult{ChatID: chatID, NodeID: nodeID, MessageID: messageID, Action: "queue-edit",
		Message: fmt.Sprintf("Edited queued message %s for node %s (chat %s).", messageID, nodeID, chatID)})
}

// RunNodeQueueRemove is `quack chat node queue-remove <chat-id> <node-id> <message-id>`:
// drop a not-yet-delivered queued message.
func RunNodeQueueRemove(ctx context.Context, out io.Writer, server, chatID, nodeID, messageID string, asJSON bool) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	if err := c.RemoveQueuedMessage(ctx, chatID, nodeID, messageID); err != nil {
		return nodeErrAs(err, chatID)
	}
	return reportAction(out, asJSON, nodeActionResult{ChatID: chatID, NodeID: nodeID, MessageID: messageID, Action: "queue-remove",
		Message: fmt.Sprintf("Removed queued message %s for node %s (chat %s).", messageID, nodeID, chatID)})
}

// RunNodeEditTask is `quack chat node edit <chat-id> <node-id> <task>`:
// replace a not-yet-started node's prompt. Errors (409) once the node has
// started - its prompt is then immutable.
func RunNodeEditTask(ctx context.Context, out io.Writer, server, chatID, nodeID, task string, asJSON bool) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	if err := c.EditNodeTask(ctx, chatID, nodeID, task); err != nil {
		return nodeErrAs(err, chatID)
	}
	return reportAction(out, asJSON, nodeActionResult{ChatID: chatID, NodeID: nodeID, Action: "edit",
		Message: fmt.Sprintf("Edited node %s's prompt (chat %s).", nodeID, chatID)})
}

// RunNodeRetry is `quack chat node retry <chat-id> <node-id> [--guidance]`:
// re-queue a finished node (done/failed/cancelled); it and everything
// downstream re-run, reusing the stored outputs of all other nodes.
func RunNodeRetry(ctx context.Context, out io.Writer, server, chatID, nodeID, guidance string, asJSON bool) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	if err := c.RetryNode(ctx, chatID, nodeID, guidance); err != nil {
		return nodeErrAs(err, chatID)
	}
	return reportAction(out, asJSON, nodeActionResult{ChatID: chatID, NodeID: nodeID, Action: "retry",
		Message: fmt.Sprintf("Retrying node %s (chat %s) - watch it with `quack chat show %s -f`.", nodeID, chatID, chatID)})
}

// RunChatDelete confirms on errOut (stderr, per house rule); an empty/closed
// stdin without yes errors rather than silently defaulting to "no".
func RunChatDelete(ctx context.Context, out, errOut io.Writer, in io.Reader, server, id string, yes, asJSON bool) error {
	c, err := NewClient(ctx, server)
	if err != nil {
		return err
	}
	if !yes {
		ok, err := confirm(errOut, in, fmt.Sprintf("Delete chat %s? This cannot be undone.", id))
		if errors.Is(err, errNonInteractive) {
			return fmt.Errorf("chat delete needs confirmation - pass -y in a non-interactive shell")
		}
		if err != nil {
			return err
		}
		if !ok {
			return reportAction(out, asJSON, nodeActionResult{ChatID: id, Action: "cancelled", Message: "Cancelled."})
		}
	}
	if err := c.DeleteChat(ctx, id); err != nil {
		return notFoundAs(err, id)
	}
	return reportAction(out, asJSON, nodeActionResult{ChatID: id, Action: "deleted", Message: fmt.Sprintf("Deleted chat %s.", id)})
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
			// without error too - check the discriminator first, or the orchestrator's raw chain-of-thought leaks in as if it were the answer (#419).
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

// nodeErrAs surfaces the server's real 404 message (via wrapNotFound); a
// node's 404 rarely means the chat itself is missing, unlike notFoundAs.
func nodeErrAs(err error, chatID string) error {
	if err == ErrNotFound {
		return fmt.Errorf("chat %s or node not found", chatID)
	}
	return err
}

// WriteJSON is the one --json encoder every command shares: every nil slice,
// top-level or nested, encodes `[]` rather than `null`.
func WriteJSON(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if !reflect.ValueOf(v).IsValid() {
		return enc.Encode(v)
	}
	return enc.Encode(denullSlices(reflect.ValueOf(v)).Interface())
}

var (
	jsonMarshalerType = reflect.TypeFor[json.Marshaler]()
	textMarshalerType = reflect.TypeFor[encoding.TextMarshaler]()
)

// denullSlices deep-copies v, nil slices to empty; a type with its own
// MarshalJSON or MarshalText (time.Time, a netip.Addr-shaped type with
// unexported state) is left untouched - a reflection-based deep copy would
// zero unexported fields a real json.Marshal never touches.
func denullSlices(v reflect.Value) reflect.Value {
	if v.Type().Implements(jsonMarshalerType) || reflect.PointerTo(v.Type()).Implements(jsonMarshalerType) ||
		v.Type().Implements(textMarshalerType) || reflect.PointerTo(v.Type()).Implements(textMarshalerType) {
		return v
	}
	switch v.Kind() {
	case reflect.Ptr:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type().Elem())
		out.Elem().Set(denullSlices(v.Elem()))
		return out
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		out := reflect.New(v.Type()).Elem()
		out.Set(denullSlices(v.Elem()))
		return out
	case reflect.Slice:
		if v.IsNil() {
			return reflect.MakeSlice(v.Type(), 0, 0)
		}
		out := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			out.Index(i).Set(denullSlices(v.Index(i)))
		}
		return out
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		out := reflect.MakeMapWithSize(v.Type(), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			out.SetMapIndex(iter.Key(), denullSlices(iter.Value()))
		}
		return out
	case reflect.Struct:
		out := reflect.New(v.Type()).Elem()
		for i := range v.NumField() {
			if v.Type().Field(i).PkgPath != "" {
				continue // unexported: encoding/json never sees it either
			}
			out.Field(i).Set(denullSlices(v.Field(i)))
		}
		return out
	default:
		return v
	}
}

// errNonInteractive: stdin had no bytes at all, distinct from a blank line
// a human typed (which still defaults to no).
var errNonInteractive = errors.New("no interactive stdin to confirm on")

// confirm reads a yes/no answer; a blank line defaults to no, an empty
// stdin returns errNonInteractive instead.
func confirm(out io.Writer, in io.Reader, prompt string) (bool, error) {
	fmt.Fprintf(out, "%s [y/N] ", prompt)
	var answer string
	if _, err := fmt.Fscanln(in, &answer); err != nil {
		if errors.Is(err, io.EOF) {
			return false, errNonInteractive
		}
		return false, nil
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "y" || answer == "yes", nil
}
