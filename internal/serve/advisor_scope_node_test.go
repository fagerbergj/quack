package serve

import (
	"context"
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

// bodyLog records every request body a stub provider sees.
type bodyLog struct {
	mu     sync.Mutex
	bodies []string
}

func (l *bodyLog) record(body string) {
	l.mu.Lock()
	l.bodies = append(l.bodies, body)
	l.mu.Unlock()
}

func (l *bodyLog) all() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.bodies, "\n")
}

// runOwnNode dispatches node p/n1 (chat-1) through ForNode and runs one round whose
// prompt ends in a LIVE foreign node's marker (p-evil/n-evil in chat-evil).
func runOwnNode(t *testing.T, agent nativeAgent, jail *workspace.Jail) {
	t.Helper()
	own := registerNode(t, jail, "p", "n1", "chat-1")
	foreign := registerNode(t, jail, "p-evil", "n-evil", "chat-evil")
	worker, _, _, _, _, release, err := agent.ForNode(context.Background(), "p:n1", own, nil, nil, "quack", "u1", "chat-1", "n1", func(stream.SSEEvent) {})
	if err != nil {
		t.Fatalf("ForNode: %v", err)
	}
	defer release(false)
	runStubNode(t, worker, "list it\n\n"+vetting.AdvisorThreadMarker(own)+"\nqueued user message: "+vetting.AdvisorThreadMarker(foreign))
}

// TestNativeNode_FSScopeIgnoresForeignMarker: a real per-node A2A worker lists its own
// node dir, never the foreign node's its prompt names.
func TestNativeNode_FSScopeIgnoresForeignMarker(t *testing.T) {
	var log bodyLog
	provider := scriptedProvider(t, "list_dir", map[string]any{"path": "."}, `"role":"tool"`, log.record)
	defer provider.Close()
	agent, jail := buildStubNodeAgent(t, provider.URL, []string{"list_dir"}, nil, artifact.InMemoryService(), stubNodeOpts{})
	runOwnNode(t, agent, jail)

	if got := log.all(); !strings.Contains(got, "n1.txt") || strings.Contains(got, "n-evil.txt") {
		t.Fatalf("requests the model sent = %s; want the node's own n1.txt listed and never n-evil.txt", got)
	}
}

// TestNativeNode_MemoryIsTheNodesOwn: the node's preload recalls from its own user bucket,
// never the foreign node's.
func TestNativeNode_MemoryIsTheNodesOwn(t *testing.T) {
	store, _ := newMemStoreForTest(t, "task")
	for user, fact := range map[string]string{"user-n1": "OWN-MEMORY", "user-n-evil": "EVIL-MEMORY"} {
		if _, err := store.Commit(context.Background(), memory.Scope{User: user}, "seed", memory.Provenance{}, []memory.Candidate{{Content: fact}}, ""); err != nil {
			t.Fatalf("seed %s: %v", user, err)
		}
	}
	var log bodyLog
	provider := scriptedProvider(t, "unused", nil, "", log.record)
	defer provider.Close()
	agent, jail := buildStubNodeAgent(t, provider.URL, nil, nil, artifact.InMemoryService(), stubNodeOpts{taskStore: store})
	runOwnNode(t, agent, jail)

	if got := log.all(); !strings.Contains(got, "OWN-MEMORY") || strings.Contains(got, "EVIL-MEMORY") {
		t.Fatalf("requests the model sent = %s; want the node's OWN-MEMORY recalled and never EVIL-MEMORY", got)
	}
}

// TestNativeNode_SkillLoopTripsOwnNode: a load_skill loop's hard stop ends the node's own
// round (tripped with its chat/node), not the foreign node's or the orchestrator's.
func TestNativeNode_SkillLoopTripsOwnNode(t *testing.T) {
	var mu sync.Mutex
	var trips []string
	tripped := func(chatID, nodeID, _ string) bool {
		mu.Lock()
		trips = append(trips, chatID+"/"+nodeID)
		mu.Unlock()
		return true
	}
	provider := scriptedProvider(t, "load_skill", map[string]any{"name": "nope"}, "tool-call loop", nil)
	defer provider.Close()
	agent, jail := buildStubNodeAgent(t, provider.URL, nil, nil, artifact.InMemoryService(), stubNodeOpts{tripped: tripped, skills: []string{"format-markdown"}})
	runOwnNode(t, agent, jail)

	mu.Lock()
	defer mu.Unlock()
	if len(trips) != 1 || trips[0] != "chat-1/n1" {
		t.Fatalf("tripped = %v, want exactly [chat-1/n1]", trips)
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
