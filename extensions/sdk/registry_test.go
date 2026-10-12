package sdk_test

import (
	"context"
	"testing"

	"github.com/fagerbergj/quack-extensions/sdk"
	"github.com/go-chi/chi/v5"
	"google.golang.org/adk/v2/tool"
)

func stubFactory(sdk.Host, []byte) (sdk.Extension, error) {
	return nil, nil
}

// registry state is package-global, so each test picks a unique name.

func TestRegisterAndRegistered(t *testing.T) {
	sdk.Register("test-register-and-registered", stubFactory)

	got := sdk.Registered()
	if _, ok := got["test-register-and-registered"]; !ok {
		t.Fatal("Registered() missing the name just registered")
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	sdk.Register("test-duplicate", stubFactory)

	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate registration, got none")
		}
	}()
	sdk.Register("test-duplicate", stubFactory)
}

func TestRegisteredReturnsCopyNotLiveMap(t *testing.T) {
	sdk.Register("test-isolation", stubFactory)

	snapshot := sdk.Registered()
	delete(snapshot, "test-isolation")

	again := sdk.Registered()
	if _, ok := again["test-isolation"]; !ok {
		t.Fatal("mutating a Registered() snapshot affected the live registry")
	}
}

// --- compile-level test: a fake extension satisfying every optional
// interface, exercised only through the public API. ---

type fakeExtension struct {
	started  bool
	runEnded []string
}

var (
	_ sdk.Extension   = (*fakeExtension)(nil)
	_ sdk.Starter     = (*fakeExtension)(nil)
	_ sdk.RunObserver = (*fakeExtension)(nil)
)

func (f *fakeExtension) Tools() []tool.Tool                       { return nil }
func (f *fakeExtension) RegisterRoutes(authed, public chi.Router) {}
func (f *fakeExtension) Start(ctx context.Context) error          { f.started = true; return nil }
func (f *fakeExtension) RunEnded(chatID string, outcome sdk.RunOutcome) {
	f.runEnded = append(f.runEnded, chatID)
}

func TestFakeExtensionSatisfiesOptionalInterfaces(t *testing.T) {
	f := &fakeExtension{}

	var ext sdk.Extension = f
	ext.RegisterRoutes(nil, nil)

	if s, ok := ext.(sdk.Starter); ok {
		if err := s.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
	} else {
		t.Fatal("fakeExtension does not satisfy sdk.Starter")
	}

	if o, ok := ext.(sdk.RunObserver); ok {
		o.RunEnded("chat-1", sdk.RunOutcome{Status: sdk.RunDone})
	} else {
		t.Fatal("fakeExtension does not satisfy sdk.RunObserver")
	}

	if !f.started || len(f.runEnded) != 1 || f.runEnded[0] != "chat-1" {
		t.Fatalf("unexpected fake state: %+v", f)
	}
}
