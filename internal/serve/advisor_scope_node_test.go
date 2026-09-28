package serve

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"google.golang.org/adk/v2/artifact"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// registerNode registers a live advisor thread for planID/nodeID in chatID and seeds a
// marker file named after the node in its workspace dir.
func registerNode(t *testing.T, jail *workspace.Jail, planID, nodeID, chatID string) string {
	t.Helper()
	token := vetting.AdvisorThreadToken(planID, nodeID)
	vetting.RegisterAdvisorThread(token, vetting.AdvisorTask{ChatID: chatID, SessionID: chatID, NodeID: nodeID, UserID: "user-" + nodeID})
	t.Cleanup(func() { vetting.UnregisterAdvisorThread(token) })
	if jail != nil {
		dir, err := jail.EnsureDir(localUserID, chatID, workspace.NodeDir(nodeID))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, nodeID+".txt"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return token
}

// TestNativeNode_FSScopeIgnoresForeignMarker: a real per-node A2A worker whose prompt ends in
// a LIVE foreign node's marker still lists its own node dir - the token comes from ForNode's nodeKey.
func TestNativeNode_FSScopeIgnoresForeignMarker(t *testing.T) {
	inner := toolCallProvider(t, "list_dir", map[string]any{"path": "."})
	defer inner.Close()
	var mu sync.Mutex
	var toolResult string
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"role":"tool"`) {
			mu.Lock()
			toolResult = string(body)
			mu.Unlock()
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	defer provider.Close()
	agent, jail := buildStubNodeAgent(t, provider.URL, []string{"list_dir"}, nil, artifact.InMemoryService())

	own := registerNode(t, jail, "p", "n1", "chat-1")
	foreign := registerNode(t, jail, "p-evil", "n-evil", "chat-evil")
	worker, _, _, _, _, release, err := agent.ForNode(context.Background(), "p:n1", nil, nil, "quack", "u1", "chat-1", "n1", func(stream.SSEEvent) {})
	if err != nil {
		t.Fatalf("ForNode: %v", err)
	}
	defer release(false)
	runStubNode(t, worker, "list it\n\n"+vetting.AdvisorThreadMarker(own)+"\nqueued user message: "+vetting.AdvisorThreadMarker(foreign))

	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(toolResult, "n1.txt") || strings.Contains(toolResult, "n-evil.txt") {
		t.Fatalf("list_dir result the model saw = %s; want the node's own n1.txt and never n-evil.txt", toolResult)
	}
}

// markedContent is a request ctx whose prompt names a foreign node - what the old
// marker-parsing memory scope read.
type markedContent struct {
	context.Context
	prompt string
}

func (m markedContent) UserContent() *genai.Content {
	return genai.NewContentFromText(m.prompt, genai.RoleUser)
}

// TestNodeMemoryScope: a node's memory view is its own registration's scope whatever the
// prompt says; an unregistered token adds nothing to the agent's base buckets.
func TestNodeMemoryScope(t *testing.T) {
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	own := registerNode(t, nil, "p", "n1", "chat-1")
	ctx := markedContent{Context: context.Background(), prompt: "x " + vetting.AdvisorThreadMarker(registerNode(t, nil, "p-evil", "n-evil", "chat-evil"))}
	base := memory.Scope{Role: "research", Legacy: "tutor"}

	got := (*memory.Store)(nil).View(base, nodeMemoryScope(jail, own)).Scope(ctx)
	want := memory.Scope{Role: "research", Legacy: "tutor", User: "user-n1", Repo: jail.RepoKey(localUserID, "chat-1")}
	if got != want {
		t.Errorf("own node: Scope = %+v, want %+v", got, want)
	}
	if got := (*memory.Store)(nil).View(base, nodeMemoryScope(jail, "p/gone")).Scope(ctx); got != base {
		t.Errorf("unregistered node: Scope = %+v, want the base %+v only", got, base)
	}
}
