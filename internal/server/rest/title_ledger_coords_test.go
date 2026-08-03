package rest

import (
	"context"
	"testing"

	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/fagerbergj/quack/internal/inference"
	"github.com/fagerbergj/quack/internal/otelobs"
)

// TestGenerateTitle_ChatEventCarriesChatID pins #617's titler entry point:
// the titler calls GenerateContent directly (no ADK runner at all), so its
// "chat" ledger event must carry the chat's ChatID instead of falling back
// to "unscoped".
func TestGenerateTitle_ChatEventCarriesChatID(t *testing.T) {
	capExp := &recordCaptureExporter{}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(capExp)))
	restore := otelobs.SetLoggerProviderForTesting(lp)
	defer restore()

	h := &Handler{titler: inference.TracedModelForTesting(stubModel{}, "titler-test-model")}

	const chatID = "titler-chat"
	if title := h.generateTitle(context.Background(), chatID, "are ducks birds?"); title == "" {
		t.Fatal("generateTitle returned empty title")
	}

	var gotChatID string
	var found bool
	for _, r := range capExp.records {
		var operation string
		r.WalkAttributes(func(kv otellog.KeyValue) bool {
			switch kv.Key {
			case otelobs.GenAIOperationName:
				operation = kv.Value.AsString()
			case otelobs.GenAIConversationID:
				gotChatID = kv.Value.AsString()
			}
			return true
		})
		if operation == otelobs.GenAIOperationChat {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("no chat ledger event recorded for the titler's call")
	}
	if gotChatID != chatID {
		t.Errorf("titler chat gen_ai.conversation.id = %q, want %q", gotChatID, chatID)
	}
}

// recordCaptureExporter records every emitted log record for direct
// inspection - a local duplicate of nodestatus_test.go-adjacent patterns
// used elsewhere in this repo (dag_test's ledgerCaptureExporter).
type recordCaptureExporter struct{ records []sdklog.Record }

func (c *recordCaptureExporter) Export(_ context.Context, records []sdklog.Record) error {
	c.records = append(c.records, records...)
	return nil
}
func (c *recordCaptureExporter) Shutdown(context.Context) error   { return nil }
func (c *recordCaptureExporter) ForceFlush(context.Context) error { return nil }
