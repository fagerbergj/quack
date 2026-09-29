package vetting

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"sync/atomic"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/agent/workflowagent"
	"google.golang.org/adk/v2/artifact"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/recordstore"
)

// TestScanSkipsForeignNodes: the session is chat-wide; a sibling's fetch must not back
// this node's citation, while an upstream node's and the orchestrator's still do.
func TestScanSkipsForeignNodes(t *testing.T) {
	fetch := func(id, u string) evtPart {
		return fnResp(id, "web_fetch", map[string]any{"results": []any{map[string]any{"url": u, "text": "page"}}})
	}
	sess := newTestSession(t, fetch("1", "https://own.test/a"), fetch("2", "https://sib.test/b"), fetch("3", "https://up.test/c"), fetch("4", "https://orch.test/d"))
	paths := []string{"root@1/web-researcher-1@r1", "root@1/web-researcher-2@r1", "root@1/research-0@r1", ""}
	i := 0
	for ev := range sess.Events().All() {
		if paths[i] != "" {
			ev.NodeInfo = &session.NodeInfo{Path: paths[i]}
		}
		i++
	}
	act := scanSessionActivity(sess, "", "web-researcher-1", false, []string{"web-researcher-2"})
	for _, u := range []string{"https://own.test/a", "https://up.test/c", "https://orch.test/d"} {
		if _, ok := act.fetched[u]; !ok {
			t.Errorf("fetched lacks %s: %v", u, act.fetched)
		}
	}
	if _, ok := act.fetched["https://sib.test/b"]; ok {
		t.Fatalf("a sibling's fetch reached this node's activity: %v", act.fetched)
	}
	if s, _, _ := citationScore("Figure ([s](https://sib.test/b)).", act); s != 0 {
		t.Errorf("cites_sources for a sibling-only page = %v, want 0", s)
	}
	if s, _, _ := citationScore("Figure ([s](https://sib.test/b)).", scanSessionActivity(sess, "", "web-researcher-1", false, nil)); s != 1 {
		t.Errorf("without ForeignNodes the session-wide scan backs it (= %v); the test no longer proves the skip", s)
	}
}

// TestCitationScore_ReadStoredPageBacksCitation: reading another node's stored copy of a
// page is retrieval by this node, so it backs the citation like a fetch.
func TestCitationScore_ReadStoredPageBacksCitation(t *testing.T) {
	u := "https://sib.test/b"
	act := workerActivity{fetched: map[string]struct{}{"https://own.test/a": {}}, seen: map[string]string{}, sourceReads: []string{pageID(t, u)}}
	if s, _, _ := citationScore("Figure ([s]("+u+")).", act); s != 1 {
		t.Errorf("cites_sources = %v, want 1 for a page the node read itself", s)
	}
}

// seedScopedChat stores one sibling artifact, one upstream artifact, and a web page a
// sibling fetched first but this node fetched too (so its lineage names the sibling).
func seedScopedChat(t *testing.T) (rc *recordstore.Client, sibID, upID, pageURL string) {
	t.Helper()
	ctx := context.Background()
	rc = recordstore.New(newMetaAwareInMemory(), artifactref.AppName, "u1", "chat1")
	var err error
	if sibID, _, err = rc.SaveBlob(ctx, "text", []byte("sibling notes"), "text/plain", "sib", recordstore.Lineage{NodeID: "web-researcher-2"}); err != nil {
		t.Fatal(err)
	}
	if upID, _, err = rc.SaveBlob(ctx, "text", []byte("upstream notes"), "text/plain", "up", recordstore.Lineage{NodeID: "research-0"}); err != nil {
		t.Fatal(err)
	}
	pageURL = "https://shared.test/page"
	page := strings.Repeat("a long line of page text that goes on for a while. ", 400) // ~20KB, one line
	if _, _, err = rc.SaveBlob(ctx, webPageKind, []byte(page), "text/markdown", pageURL, recordstore.Lineage{NodeID: "web-researcher-2", SourceURL: pageURL}); err != nil {
		t.Fatal(err)
	}
	return rc, sibID, upID, pageURL
}

// TestJudgeArtifactTools_ScopeAndBudget: within a judge round the tools hide a sibling's
// artifacts, keep upstream and self-fetched ones, and cap source-page reads.
func TestJudgeArtifactTools_ScopeAndBudget(t *testing.T) {
	rc, sibID, upID, pageURL := seedScopedChat(t)
	tools, err := NewJudgeArtifactTools(rc)
	if err != nil {
		t.Fatal(err)
	}
	list, read := tools[0].(runnableTool), tools[1].(runnableTool)
	act := workerActivity{fetched: map[string]struct{}{pageURL: {}}}
	view := &judgeView{foreign: map[string]bool{"web-researcher-2": true}, own: act.ownArtifactIDs()}
	ctx := &judgeToolCtx{StrictContextMock: adkagent.NewStrictContextMock(withJudgeView(context.Background(), view))}

	out, err := list.Run(ctx, map[string]any{})
	listing, _ := out["result"].(string)
	if err != nil || strings.Contains(listing, sibID) || !strings.Contains(listing, upID) || !strings.Contains(listing, pageID(t, pageURL)) {
		t.Errorf("list_artifacts = %q (err %v), want upstream and the self-fetched page, not %s", listing, err, sibID)
	}
	if _, err := read.Run(ctx, map[string]any{"id": sibID}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("read of a sibling artifact: err = %v, want not found", err)
	}
	for i := 1; i <= judgeSourceReadBudget; i++ {
		out, err := read.Run(ctx, map[string]any{"id": pageID(t, pageURL), "offset": float64(1)})
		if body, _ := out["result"].(string); err != nil || len(body) > judgeSourceReadCap+100 || strings.Contains(body, "budget spent") {
			t.Fatalf("source read %d: %d chars, err %v, want a read within %d chars plus its footer", i, len(body), err, judgeSourceReadCap)
		}
	}
	out, _ = read.Run(ctx, map[string]any{"id": pageID(t, pageURL)})
	if body, _ := out["result"].(string); !strings.Contains(body, "read budget spent") {
		t.Errorf("read %d of a source page = %.80q, want the budget refusal", judgeSourceReadBudget+1, body)
	}
	if out, err := read.Run(ctx, map[string]any{"id": upID}); err != nil || out["result"] != "upstream notes" {
		t.Errorf("a non-source read after the budget = %v, %v, want it unaffected", out["result"], err)
	}
}

// listingJudge calls list_artifacts once, records what came back, then passes.
type listingJudge struct{ listing *string }

func (listingJudge) Name() string { return "listing-judge" }

func (j listingJudge) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		for _, c := range req.Contents {
			for _, p := range c.Parts {
				if p.FunctionResponse != nil && p.FunctionResponse.Name == "list_artifacts" {
					*j.listing, _ = p.FunctionResponse.Response["result"].(string)
					yield(stubCall(submitVerdictTool, map[string]any{"score": 0.9, "feedback": ""}), nil)
					return
				}
			}
		}
		yield(stubCall("list_artifacts", map[string]any{}), nil)
	}
}

// TestRunJudgeRound_ScopesArtifactToolsToNode: a real judge round applies cfg.ForeignNodes
// to its artifact tools, so the judge never sees a sibling's artifact.
func TestRunJudgeRound_ScopesArtifactToolsToNode(t *testing.T) {
	rc, sibID, upID, _ := seedScopedChat(t)
	tools, err := NewJudgeArtifactTools(rc)
	if err != nil {
		t.Fatal(err)
	}
	var listing string
	cfg := Config{Rubric: "score 0-10", JudgeArtifactTools: tools, ForeignNodes: []string{"web-researcher-2"}}
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Research X"}}}
	if _, err := runJudgeAgent(t.Context(), NewJudgeFactory(listingJudge{listing: &listing}, nil, nil), cfg, q, "answer", workerActivity{}, nil, nil, func(*genai.Part) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(listing, sibID) || !strings.Contains(listing, upID) {
		t.Errorf("judge's list_artifacts = %q, want %s visible and %s hidden", listing, upID, sibID)
	}
}

// TestRunGatedRefine_JudgePromptCarriesCitedEvidence: with specifics_supported declared,
// the verify tier runs before the judge and its per-claim section reaches the judge prompt.
func TestRunGatedRefine_JudgePromptCarriesCitedEvidence(t *testing.T) {
	judgeStub := &judgePromptCapturingModel{}
	answer := "Users rose 25% in 2024 ([survey](https://example.test/survey))."
	var verifierCalls []string
	cfg := Config{JudgeRounds: 1, Threshold: 0.5, Rubric: "score the answer 0-10", RecordReader: fakeLoader{},
		RubricSpecs: map[string]criterionSpec{specificsSupportedCriterion: {Deterministic: true}},
		JudgeModel:  seqLLM{answers: []string{`{"items":[]}`}, prompts: &verifierCalls}}
	runGatedStub(t, answer, NewJudgeFactory(judgeStub, nil, nil), cfg)
	if len(judgeStub.prompts) == 0 {
		t.Fatal("judge was never called")
	}
	first := judgeStub.prompts[0]
	ev, ans := strings.Index(first, "CITED EVIDENCE"), strings.Index(first, "Answer to judge:")
	if ev < 0 || ans < ev || !strings.Contains(first[ev:ans], `"25%"`) || !strings.Contains(first[ev:ans], "no stored text") {
		t.Errorf("judge prompt lacks the cited-evidence section ahead of the answer:\n%s", first)
	}
}

// runGatedStub runs one gated node whose worker always answers answer.
func runGatedStub(t *testing.T, answer string, judge JudgeFactory, cfg Config) GateResult {
	t.Helper()
	worker, err := llmagent.New(llmagent.Config{Name: "web-researcher", Model: stubFixedAnswerModel{text: answer}, Description: "researcher", Instruction: "Answer."})
	if err != nil {
		t.Fatal(err)
	}
	var res GateResult
	node, err := newTestGatedNodeCapture("researcher-gate", worker, stubFixedAnswerModel{}, judge, cfg, &res)
	if err != nil {
		t.Fatal(err)
	}
	root, err := workflowagent.New(workflowagent.Config{Name: "root", SubAgents: []adkagent.Agent{worker}, Edges: workflow.Chain(workflow.Start, node)})
	if err != nil {
		t.Fatal(err)
	}
	r, err := runner.New(runner.Config{AppName: "test", Agent: root, SessionService: session.InMemoryService(), AutoCreateSession: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range r.Run(t.Context(), "u", "s", &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Research the survey."}}}, adkagent.RunConfig{}) {
		if err != nil {
			t.Fatal(err)
		}
	}
	return res
}

// rejudgeJudge reads a file, then answers with an unscored criterion; the gate's re-judge
// (a fresh session) is captured and scored.
type rejudgeJudge struct {
	calls  int32
	second string
}

func (*rejudgeJudge) Name() string { return "rejudge-judge" }

func (j *rejudgeJudge) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		crit := map[string]any{"shortfall": "fine"}
		switch atomic.AddInt32(&j.calls, 1) {
		case 1:
			yield(stubCall("read_file", map[string]any{"path": "game.go"}), nil)
			return
		case 2:
		default:
			j.second = stubAllText(req)
			crit["score"] = 3.0
		}
		raw, _ := json.Marshal(map[string]any{"score": 3.0, "criteria": map[string]any{"accuracy": crit}, "feedback": ""})
		yield(stubText(string(raw)), nil)
	}
}

// TestReJudge_NoteOutsideAnswerAndSeededReads: the gate's reason for a re-judge sits outside
// "Answer to judge:", and the fresh session starts with the reads the first attempt made.
func TestReJudge_NoteOutsideAnswerAndSeededReads(t *testing.T) {
	judge := &rejudgeJudge{}
	readTool := newSpyReadTool(t, "package game // the body", new(int32))
	cfg := Config{Rubric: "score 0-3", JudgeMaxIterations: 6, Threshold: 0.6, RubricSpecs: map[string]criterionSpec{"accuracy": {Name: "accuracy"}}}
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement game.go"}}}
	if _, err := runJudgeAgent(t.Context(), NewJudgeFactory(judge, []tool.Tool{readTool}, nil), cfg, q, "done.", workerActivity{}, nil, nil, func(*genai.Part) bool { return true }); err != nil {
		t.Fatal(err)
	}
	_, after, ok := strings.Cut(judge.second, "Answer to judge:\n")
	if !ok || strings.TrimSpace(after) != "done." {
		t.Errorf("re-judge's answer section = %q, want exactly the answer", after)
	}
	head := strings.Split(judge.second, "Answer to judge:")[0]
	if !strings.Contains(head, "NOTE FROM THE GATE") || !strings.Contains(head, "no score") {
		t.Errorf("re-judge prompt lacks the gate's note ahead of the answer:\n%s", judge.second)
	}
	if !strings.Contains(head, "READS FROM YOUR PREVIOUS ATTEMPT") || !strings.Contains(head, "package game // the body") {
		t.Errorf("re-judge prompt lacks the first attempt's read:\n%s", judge.second)
	}
}

// TestStoredPageIDsAndRevisePrompt: the revise prompt names the stored copies of the
// pages the worker fetched, so it re-reads them instead of fetching them again.
func TestStoredPageIDsAndRevisePrompt(t *testing.T) {
	svc := artifact.InMemoryService()
	rc := recordstore.New(svc, artifactref.AppName, "u1", "chat1")
	stored, inline := "https://x.test/stored", "https://x.test/inline"
	if _, _, err := rc.SaveBlob(context.Background(), webPageKind, []byte("page"), "text/markdown", stored, recordstore.Lineage{SourceURL: stored}); err != nil {
		t.Fatal(err)
	}
	act := workerActivity{fetched: map[string]struct{}{stored: {}, inline: {}}}
	ids := storedPageIDs(context.Background(), Config{Artifacts: svc, ChatID: "chat1", User: "u1"}, act)
	if len(ids) != 1 || ids[0] != pageID(t, stored) {
		t.Fatalf("storedPageIDs = %v, want only %s", ids, pageID(t, stored))
	}
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Research X"}}}
	got := contentPlainText(buildRevisionContent("", q, "answer", verdictEnvelope{}, act, false, nil, ids))
	if !strings.Contains(got, ids[0]) || !strings.Contains(got, "instead of calling web_fetch again") || strings.Contains(got, "re-fetch") {
		t.Errorf("revise prompt does not steer to the stored pages:\n%s", got)
	}
}

// TestJudgePromptBoundedWithManyPages: a node that fetched 30 large pages and cites them
// 60 times gives the judge a bounded evidence section, never the pages themselves.
func TestJudgePromptBoundedWithManyPages(t *testing.T) {
	pages := fakeLoader{}
	act := workerActivity{fetched: map[string]struct{}{}, seen: map[string]string{}}
	var answer strings.Builder
	for i := range 30 {
		u := "https://news.test/story-" + string(rune('a'+i%26)) + strings.Repeat("x", i/26)
		pages[pageID(t, u)] = strings.Repeat("unrelated filler sentence about the market. ", 1100) + "revenue grew 12% and 34% overall."
		act.fetched[u] = struct{}{}
		answer.WriteString("Revenue grew 12% and 34% overall ([s](" + u + ")).\n\n")
	}
	var calls []string
	cfg := Config{Rubric: "score 0-3", RecordReader: pages, RubricSpecs: map[string]criterionSpec{specificsSupportedCriterion: {Deterministic: true}},
		JudgeModel: seqLLM{answers: []string{`{"items":[]}`}, prompts: &calls}}
	section := judgeEvidenceSection(runVerify(context.Background(), cfg, answer.String(), act, map[string]Verdict{}))
	cfg.judgeEvidence = section
	prompt := buildJudgePrompt("", cfg.Rubric, "task", "", &genai.Content{Parts: []*genai.Part{{Text: "q"}}}, answer.String(), "", act, joinSections("", cfg.judgeEvidence))
	fixed := len(prompt) - answer.Len()
	if len(section) > judgeEvidenceChars+200 || fixed > judgeEvidenceChars+1_000 {
		t.Errorf("evidence %d chars, prompt beyond the answer %d chars (~%d tokens), want both bounded near %d", len(section), fixed, fixed/judgeCharsPerToken, judgeEvidenceChars)
	}
	if strings.Count(prompt, "unrelated filler sentence") > 60*judgeExcerptChars/40 {
		t.Error("page text reached the prompt beyond the per-claim excerpts")
	}
}

// TestPriorReadsSectionFitsRoom: seeded reads never push a retry past the room its prompt has left.
func TestPriorReadsSectionFitsRoom(t *testing.T) {
	reads := []string{strings.Repeat("a", 3000), strings.Repeat("b", 3000)}
	c := judgeReadCounters{reads: &reads}
	if got := priorReadsSection(c, 100); got != "" {
		t.Errorf("no room: section = %.60q, want none", got)
	}
	got := priorReadsSection(c, 4000)
	if len(got) > 4100 || !strings.Contains(got, "(1 later reads not shown)") {
		t.Errorf("section = %d chars, want the first read and a note for the second", len(got))
	}
}

// noVerdictThenSeededJudge reads a file, then never reaches a verdict in that round; the
// fresh-session retry is captured and passes without reading again.
type noVerdictThenSeededJudge struct {
	fresh int32
	retry string
}

func (*noVerdictThenSeededJudge) Name() string { return "no-verdict-then-seeded-judge" }

func (j *noVerdictThenSeededJudge) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		switch {
		case !isFreshRound(req):
			yield(stubText("still thinking about it"), nil)
		case atomic.AddInt32(&j.fresh, 1) == 1:
			yield(stubCall("read_file", map[string]any{"path": "game.go"}), nil)
		default:
			j.retry = stubAllText(req)
			yield(stubText(`{"score": 0.9, "passed": true, "feedback": ""}`), nil)
		}
	}
}

// TestRetryNoVerdict_SeedsAndCreditsPriorReads: the retry starts with the first attempt's
// reads and counts them, so a pass built on them is not discarded as unread.
func TestRetryNoVerdict_SeedsAndCreditsPriorReads(t *testing.T) {
	judge := &noVerdictThenSeededJudge{}
	readTool := newSpyReadTool(t, "package game // retry body", new(int32))
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Implement game.go"}}}
	v, err := runJudgeAgent(t.Context(), NewJudgeFactory(judge, []tool.Tool{readTool}, nil), Config{Rubric: "score 0-10", JudgeMaxIterations: 6}, q, "done.", workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
	if err != nil || !v.Passed {
		t.Fatalf("runJudgeAgent = %+v, %v, want the retry's pass", v, err)
	}
	if !strings.Contains(judge.retry, "READS FROM YOUR PREVIOUS ATTEMPT") || !strings.Contains(judge.retry, "retry body") {
		t.Errorf("retry prompt lacks the first attempt's read:\n%s", judge.retry)
	}
	if got := atomic.LoadInt32(&judge.fresh); got != 2 {
		t.Errorf("fresh judge sessions = %d, want 2 (a seeded pass must not be re-judged as unread)", got)
	}
}
