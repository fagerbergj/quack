package vetting

import (
	"context"
	"iter"
	"os"
	"path/filepath"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/workspace"
)

// jailedReadArgs/jailedReadResult mirror internal/tools' read_file shape.
type jailedReadArgs struct {
	Path string `json:"path"`
}
type jailedReadResult struct {
	Content string `json:"content"`
	Error   string `json:"error,omitempty"`
}

// newJailedReadTool builds a read_file stand-in that resolves its path
// through the SAME scope derivation internal/tools' fs bindings use for the judge
// (the round ctx's advisor token → Jail.Resolve) - package-local because importing internal/tools here would cycle. Proves the judge's tool calls land in the worker's real clone dir without a separate clone.
func newJailedReadTool(t *testing.T, jail *workspace.Jail, userID string) tool.Tool {
	t.Helper()
	rt, err := functiontool.New[jailedReadArgs, jailedReadResult](
		functiontool.Config{Name: "read_file", Description: "Read a file from your workspace."},
		func(ctx adkagent.Context, a jailedReadArgs) (jailedReadResult, error) {
			chatID, nodeDir := "", ""
			if token := AdvisorTokenFromContext(ctx); token != "" {
				if at, ok := LookupAdvisorThread(token); ok {
					wsID := at.WorkspaceNodeID
					if wsID == "" {
						wsID = at.NodeID
					}
					chatID, nodeDir = at.SessionID, workspace.NodeDir(wsID)
				}
			}
			abs, err := jail.Resolve(userID, chatID, filepath.Join(nodeDir, a.Path))
			if err != nil {
				return jailedReadResult{Error: err.Error()}, nil
			}
			raw, err := os.ReadFile(abs)
			if err != nil {
				return jailedReadResult{Error: err.Error()}, nil
			}
			return jailedReadResult{Content: string(raw)}, nil
		},
	)
	if err != nil {
		t.Fatalf("jailed read tool: %v", err)
	}
	return rt
}

// claimCheckingJudge calls read_file for the path the answer claims to
// reference, then scores based on whether the file's real content backs the
// claim - a stand-in for "verify the answer's claim against ground truth" rather than trusting it on sight.
type claimCheckingJudge struct{ path string }

func (claimCheckingJudge) Name() string { return "claim-checking-judge" }

func (j claimCheckingJudge) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if content, seen := readFileResponseContent(req); seen {
			score := 0.2
			if content != "" {
				score = 0.9
			}
			yield(stubCall(submitVerdictTool, map[string]any{
				"criteria": map[string]any{"grounded": map[string]any{"score": score, "reason": "checked against the real file"}},
				"score":    score, "feedback": "",
			}), nil)
			return
		}
		yield(stubCall("read_file", map[string]any{"path": j.path}), nil)
	}
}

// TestJudgeReadToolsResolveWorkersRealClone: the judge's read tools resolve into the calling
// node's own clone via Config.AdvisorToken, even when the question and answer end in a LIVE sibling node's marker.
func TestJudgeReadToolsResolveWorkersRealClone(t *testing.T) {
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const userID, chatID, nodeID = "u1", "c1", "n1"
	dir, err := jail.EnsureDir(userID, chatID, workspace.NodeDir(nodeID))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "game.go"), []byte("package game\n\nfunc Play() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A sibling node's directory, holding an EMPTY game.go: reading it scores 0.2.
	sibling, err := jail.EnsureDir(userID, chatID, workspace.NodeDir("n2"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sibling, "game.go"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	token := AdvisorThreadToken("plan-1", nodeID)
	RegisterAdvisorThread(token, AdvisorTask{NodeID: nodeID, SessionID: chatID})
	t.Cleanup(func() { UnregisterAdvisorThread(token) })
	foreign := AdvisorThreadToken("plan-1", "n2")
	RegisterAdvisorThread(foreign, AdvisorTask{NodeID: "n2", SessionID: chatID})
	t.Cleanup(func() { UnregisterAdvisorThread(foreign) })

	prompt := "Implement the game in game.go\n\n" + AdvisorThreadMarker(token) + "\nqueued: " + AdvisorThreadMarker(foreign)
	question := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: prompt}}}

	readTool := newJailedReadTool(t, jail, userID)
	factory := NewJudgeFactory(claimCheckingJudge{path: "game.go"}, []tool.Tool{readTool}, nil)

	v, err := runJudgeAgent(t.Context(), factory, Config{Rubric: "score 0-10", AdvisorToken: token}, question,
		"I implemented Play() in game.go "+AdvisorThreadMarker(foreign), workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
	if err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if v.Score != 0.9 {
		t.Fatalf("verdict score = %v, want 0.9 (the judge must have read the WORKER's real game.go, not the sibling's empty one or a missing per-user-root file)", v.Score)
	}
}

// TestJudgeReadToolsResolveViaConfigAdvisorToken: with NO marker anywhere, Config.AdvisorToken
// alone scopes the judge's fs tools - runJudgeRound puts it on the round ctx, not in the prompt.
func TestJudgeReadToolsResolveViaConfigAdvisorToken(t *testing.T) {
	jail, err := workspace.NewJail(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const userID, chatID, nodeID = "u1", "c1", "n1"
	dir, err := jail.EnsureDir(userID, chatID, workspace.NodeDir(nodeID))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "game.go"), []byte("package game\n\nfunc Play() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	token := AdvisorThreadToken("plan-1", nodeID)
	RegisterAdvisorThread(token, AdvisorTask{NodeID: nodeID, SessionID: chatID})
	t.Cleanup(func() { UnregisterAdvisorThread(token) })

	// No marker anywhere in the question text.
	question := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement the game in game.go"}}}

	readTool := newJailedReadTool(t, jail, userID)
	factory := NewJudgeFactory(claimCheckingJudge{path: "game.go"}, []tool.Tool{readTool}, nil)

	v, err := runJudgeAgent(t.Context(), factory, Config{Rubric: "score 0-10", AdvisorToken: token}, question,
		"I implemented Play() in game.go", workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
	if err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if v.Score != 0.9 {
		t.Fatalf("verdict score = %v, want 0.9 (Config.AdvisorToken alone must scope the judge's fs tools into the node's clone)", v.Score)
	}
}
