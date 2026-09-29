package inference

import (
	"context"
	"errors"
	"iter"
	"testing"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/ledger"
)

// cancelledModel fails the way the gateway client does when its ctx is cancelled.
type cancelledModel struct{}

func (cancelledModel) Name() string { return "cancelled" }

func (cancelledModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(nil, errors.New("openai request failed: context canceled"))
	}
}

// TestTraced_CancelledCallIsNoGatewayFailure: a call cut by a user stop must not read as a
// gateway failure, or the stopped chat's outcome derives as failed.
func TestTraced_CancelledCallIsNoGatewayFailure(t *testing.T) {
	const chatID = "chat-cancel-failure"
	ctx, cancel := context.WithCancel(ledger.WithCoords(context.Background(), ledger.Coords{ChatID: chatID}))
	cancel()
	for range TracedModelForTesting(cancelledModel{}, "m").GenerateContent(ctx, &model.LLMRequest{}, false) {
	}
	if _, _, _, ok := LastFailure(chatID, "", ""); ok {
		t.Fatal("a cancelled call was recorded as a gateway failure")
	}
}
