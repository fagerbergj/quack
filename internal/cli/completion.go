package cli

import (
	"context"
	"net/url"
	"strconv"

	"github.com/fagerbergj/quack/internal/schema"
)

// completionChatLimit caps the listing like completionMemoryLimit below -
// ListChats auto-pages every chat, which could burn the whole completionTimeout mid-pagination.
const completionChatLimit = 50

// CompletionChatIDs lists chat ids for shell completion of any `<chat-id>`
// positional - a single page, unlike `chat list`'s full ListChats.
func CompletionChatIDs(ctx context.Context, server string) ([]string, error) {
	c, err := NewClient(ctx, server)
	if err != nil {
		return nil, err
	}
	var out schema.ChatList
	q := url.Values{"limit": {strconv.Itoa(completionChatLimit)}}
	if err := c.getJSON(ctx, "/api/v1/chats?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	ids := make([]string, len(out.Data))
	for i, ch := range out.Data {
		ids[i] = ch.Id
	}
	return ids, nil
}

// CompletionNodeIDs lists chatID's latest-turn node ids, for `chat node
// <verb>`'s <node-id> positional.
func CompletionNodeIDs(ctx context.Context, server, chatID string) ([]string, error) {
	c, err := NewClient(ctx, server)
	if err != nil {
		return nil, err
	}
	detail, err := c.GetChat(ctx, chatID)
	if err != nil {
		return nil, err
	}
	dagItem, ok := lastTurnDag(detail.Turns)
	if !ok {
		return nil, nil
	}
	ids := make([]string, len(dagItem.Nodes))
	for i, n := range dagItem.Nodes {
		ids[i] = n.Id
	}
	return ids, nil
}

// completionMemoryLimit caps the completion listing so a large store doesn't
// stall shell tab-completion waiting on a full auto-paged fetch.
const completionMemoryLimit = 50

// CompletionMemoryIDs lists memory ids for `memory show|forget <memory-id>`.
func CompletionMemoryIDs(ctx context.Context, server string) ([]string, error) {
	c, err := NewClient(ctx, server)
	if err != nil {
		return nil, err
	}
	list, err := c.ListMemories(ctx, "", "", "", "", completionMemoryLimit, false)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(list.Memories))
	for i, m := range list.Memories {
		ids[i] = m.Id
	}
	return ids, nil
}
