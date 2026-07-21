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

// newJailedReadTool builds a read_file stand-in that resolves its path through
// the SAME two-step scope derivation internal/tools' fs bindings use
// (scopeFromContext → Jail.Resolve): the advisor-thread marker embedded in the
// invocation's UserContent names the (chatID, nodeDir) the call runs under, via
// the process-local registry the gate populates for the WORKER. This is a
// package-local stand-in (not internal/tools' real fs binding — importing that
// package here would cycle, since it already imports vetting for
// ParseAdvisorThread/LookupAdvisorThread) that exercises the identical
// resolution rule, proving whether the judge's tool calls land in the worker's
// real clone dir without a separate clone.
func newJailedReadTool(t *testing.T, jail *workspace.Jail, userID string) tool.Tool {
	t.Helper()
	rt, err := functiontool.New[jailedReadArgs, jailedReadResult](
		functiontool.Config{Name: "read_file", Description: "Read a file from your workspace."},
		func(ctx adkagent.Context, a jailedReadArgs) (jailedReadResult, error) {
			chatID, nodeDir := "", ""
			if token, ok := ParseAdvisorThread(contentText(ctx.UserContent())); ok {
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

func contentText(c *genai.Content) string {
	if c == nil {
		return ""
	}
	var out string
	for _, p := range c.Parts {
		if p != nil && p.Text != "" {
			out += p.Text
		}
	}
	return out
}

// claimCheckingJudge calls read_file for the path the answer claims to
// reference, then scores based on whether the file's real content backs the
// claim — a stand-in for "verify the answer's claim against ground truth"
// rather than trusting it on sight.
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

// TestJudgeReadToolsResolveWorkersRealClone proves the judge's read-only
// workspace tools resolve into the SAME clone the worker used — no second
// clone, no separate jail scope — by driving the real gate plumbing: register
// an advisor thread exactly as dag.newGatedNode does for a node's worker,
// embed the resulting marker in the worker prompt (mirroring
// graph.go/node.go), and confirm the judge's read_file call (scoped purely
// from the invocation's UserContent, like a real judge round) reads the file
// the "worker" actually wrote under its OWN node directory rather than the
// per-user root or a sibling node's directory.
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
	// A sibling node's directory, holding a DIFFERENT file — proves the judge
	// reads the CALLING node's own clone, not just any clone under the chat.
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

	prompt := "Implement the game in game.go\n\n" + AdvisorThreadMarker(token)
	question := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: prompt}}}

	readTool := newJailedReadTool(t, jail, userID)
	factory := NewJudgeFactory(claimCheckingJudge{path: "game.go"}, []tool.Tool{readTool}, nil)

	v, err := runJudgeAgent(t.Context(), factory, Config{Rubric: "score 0-10"}, question,
		"I implemented Play() in game.go", workerActivity{}, func(*genai.Part) bool { return true })
	if err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if v.Score != 0.9 {
		t.Fatalf("verdict score = %v, want 0.9 (the judge must have read the WORKER's real game.go, not an empty sibling or a missing per-user-root file)", v.Score)
	}
}

// TestJudgeReadToolsResolveViaConfigAdvisorToken pins #502/#498's fix: the
// judge's own content (what runJudgeRound hands the runner as UserContent) is
// buildJudgePrompt's output, not `question` itself, so scopeFromContext must
// not depend on whatever marker `question`'s text happens to carry. Here
// `question` carries NO marker at all — resolution must come entirely from
// Config.AdvisorToken, which runJudgeRound stamps onto its own content.
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
		"I implemented Play() in game.go", workerActivity{}, func(*genai.Part) bool { return true })
	if err != nil {
		t.Fatalf("runJudgeAgent: %v", err)
	}
	if v.Score != 0.9 {
		t.Fatalf("verdict score = %v, want 0.9 (Config.AdvisorToken alone must scope the judge's fs tools into the node's clone)", v.Score)
	}
}
