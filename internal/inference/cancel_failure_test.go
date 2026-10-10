package inference

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/adk/v2/model"

	"github.com/fagerbergj/quack/internal/ledger"
)

// TestTraced_CancelledCallIsNoGatewayFailure: a call cut by a user stop must not read as a
// gateway failure, or the stopped chat's outcome derives as failed.
func TestTraced_CancelledCallIsNoGatewayFailure(t *testing.T) {
	const chatID = "chat-cancel-failure"
	ctx, cancel := context.WithCancel(ledger.WithCoords(context.Background(), ledger.Coords{ChatID: chatID}))
	cancel()
	cancelled := &stubModel{name: "cancelled", err: errors.New("openai request failed: context canceled")}
	for range TracedModelForTesting(cancelled, "m").GenerateContent(ctx, &model.LLMRequest{}, false) {
	}
	if _, _, _, ok := LastFailure(chatID, "", ""); ok {
		t.Fatal("a cancelled call was recorded as a gateway failure")
	}
}
