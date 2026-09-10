package cli

import "context"

// CompletionChatIDs lists chat ids for shell completion of any `<chat-id>`
// positional - the same call `chat list` makes.
func CompletionChatIDs(ctx context.Context, server string) ([]string, error) {
	c, err := NewClient(ctx, server)
	if err != nil {
		return nil, err
	}
	chats, err := c.ListChats(ctx, nil)
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(chats))
	for i, ch := range chats {
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
