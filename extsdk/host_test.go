package sdk_test

import (
	"context"
	"errors"
	"testing"

	"github.com/fagerbergj/quack-extensions/sdk"
)

// TestHostContextDirChatUserArchiveChat pins the v0.3.0 Host additions'
// signatures - a real caller (quack's serve package) wires these as plain
// closures, so the sdk package itself only proves the shape is callable.
func TestHostContextDirChatUserArchiveChat(t *testing.T) {
	var archived []string
	h := sdk.Host{
		EnsureContextDir: func(userID, chatID string) (string, error) {
			if userID == "" || chatID == "" {
				return "", errors.New("missing id")
			}
			return "/data/" + userID + "/" + chatID, nil
		},
		ChatUser: func(chatID string) (string, bool) {
			if chatID == "known" {
				return "alice", true
			}
			return "", false
		},
		ArchiveChat: func(chatID string) error {
			archived = append(archived, chatID)
			return nil
		},
	}

	dir, err := h.EnsureContextDir("u1", "c1")
	if err != nil || dir != "/data/u1/c1" {
		t.Errorf("EnsureContextDir(u1, c1) = (%q, %v), want (/data/u1/c1, nil)", dir, err)
	}

	if user, ok := h.ChatUser("known"); !ok || user != "alice" {
		t.Errorf("ChatUser(known) = (%q, %v), want (alice, true)", user, ok)
	}
	if _, ok := h.ChatUser("missing"); ok {
		t.Errorf("ChatUser(missing) ok = true, want false")
	}

	if err := h.ArchiveChat("c1"); err != nil {
		t.Fatalf("ArchiveChat: %v", err)
	}
	if len(archived) != 1 || archived[0] != "c1" {
		t.Errorf("archived = %v, want [c1]", archived)
	}
}

// TestHostClassifyDegradesGracefullyWhenNil pins that a nil Classify (no
// judge model configured) is a valid, expected state - callers must check
// before calling, not assume it's always wired.
func TestHostClassifyDegradesGracefullyWhenNil(t *testing.T) {
	var h sdk.Host
	if h.Classify != nil {
		t.Fatalf("zero-value Host.Classify = non-nil, want nil")
	}

	h.Classify = func(ctx context.Context, prompt string) (string, error) {
		if prompt == "" {
			return "", errors.New("empty prompt")
		}
		return "WORK", nil
	}
	answer, err := h.Classify(context.Background(), "please review this PR")
	if err != nil || answer != "WORK" {
		t.Errorf("Classify = (%q, %v), want (WORK, nil)", answer, err)
	}
}

// TestHostUpdateChatOriginDegradesGracefullyWhenNil pins that a nil
// UpdateChatOrigin (no update path configured) is a valid, expected state.
func TestHostUpdateChatOriginDegradesGracefullyWhenNil(t *testing.T) {
	var h sdk.Host
	if h.UpdateChatOrigin != nil {
		t.Fatalf("zero-value Host.UpdateChatOrigin = non-nil, want nil")
	}

	h.UpdateChatOrigin = func(ctx context.Context, chatID string, origin *sdk.ChatOrigin) error {
		return errors.New("no update path configured")
	}

	// Calling with nil origin should be valid (removes the origin).
	if err := h.UpdateChatOrigin(context.Background(), "chat-1", nil); err == nil {
		t.Error("expected error for unconfigured UpdateChatOrigin")
	}

	// Calling with an updated badge.
	expectedBadge := "closed"
	origin := &sdk.ChatOrigin{Label: "user/repo#42", Badge: expectedBadge}
	if err := h.UpdateChatOrigin(context.Background(), "chat-1", origin); err == nil {
		t.Error("expected error for unconfigured UpdateChatOrigin")
	}
}
