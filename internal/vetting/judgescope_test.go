package vetting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"strings"
	"sync"
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
	"google.golang.org/adk/v2/tool/functiontool"
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

// revisionMetaStore keeps each revision's lineage, as the production row store does;
// version 0 (a latest read) answers with the newest revision's.
type revisionMetaStore struct {
	artifact.Service
	mu   sync.Mutex
	meta map[string][]byte
}

func (m *revisionMetaStore) key(r *artifact.LoadRequest) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d", r.AppName, r.UserID, r.SessionID, r.FileName, r.Version)
}

func (m *revisionMetaStore) SaveWithMeta(ctx context.Context, req *artifact.SaveRequest, _, _ string, lineage []byte, _ string) (*artifact.SaveResponse, error) {
	resp, err := m.Service.Save(ctx, req)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, v := range []int64{0, resp.Version} {
		m.meta[m.key(&artifact.LoadRequest{AppName: req.AppName, UserID: req.UserID, SessionID: req.SessionID, FileName: req.FileName, Version: v})] = lineage
	}
	return resp, nil
}

func (m *revisionMetaStore) LoadWithMeta(ctx context.Context, req *artifact.LoadRequest) (*artifact.LoadResponse, string, string, []byte, error) {
	resp, err := m.Service.Load(ctx, req)
	if err != nil {
		return nil, "", "", nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return resp, "", "", m.meta[m.key(req)], nil
}

// seedScopedChat stores a sibling's artifact, an upstream artifact a sibling edited last, a
// web page a sibling fetched first but this node fetched too, and a bytes input.
func seedScopedChat(t *testing.T) (rc *recordstore.Client, sibID, upID, pageURL string) {
	return seedScopedChatOn(t, &revisionMetaStore{Service: artifact.InMemoryService(), meta: map[string][]byte{}})
}

func seedScopedChatOn(t *testing.T, svc artifact.Service) (rc *recordstore.Client, sibID, upID, pageURL string) {
	t.Helper()
	ctx := context.Background()
	rc = recordstore.New(svc, artifactref.AppName, "u1", "chat1")
	var err error
	if sibID, _, err = rc.SaveBlob(ctx, "text", []byte("sibling notes"), "text/plain", "sib", recordstore.Lineage{NodeID: "web-researcher-2"}); err != nil {
		t.Fatal(err)
	}
	if upID, _, err = rc.SaveBlob(ctx, "text", []byte("upstream draft"), "text/plain", "up", recordstore.Lineage{NodeID: "research-0"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = rc.SaveBlob(ctx, "text", []byte("upstream notes"), "text/plain", "up", recordstore.Lineage{NodeID: "web-researcher-2"}); err != nil {
		t.Fatal(err)
	}
	pageURL = "https://shared.test/page"
	pageID(t, pageURL)                                                                 // registers the web_page kind
	page := strings.Repeat("a long line of page text that goes on for a while. ", 400) // ~20KB, one line
	if _, _, err = rc.SaveBlob(ctx, webPageKind, []byte(page), "text/markdown", pageURL, recordstore.Lineage{NodeID: "web-researcher-2", SourceURL: pageURL}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = rc.SaveBlob(ctx, kindBytes, []byte(page), "text/plain", "files", recordstore.Lineage{}); err != nil {
		t.Fatal(err)
	}
	return rc, sibID, upID, pageURL
}

// TestJudgeArtifactTools_ScopeAndCheckedPages: within a judge round the tools hide a sibling's
// artifacts and keep upstream ones; once the verify tier read the pages, they are hidden too.
func TestJudgeArtifactTools_ScopeAndCheckedPages(t *testing.T) {
	rc, sibID, upID, pageURL := seedScopedChat(t)
	tools, err := NewJudgeArtifactTools(rc)
	if err != nil {
		t.Fatal(err)
	}
	list, read := tools[0].(runnableTool), tools[1].(runnableTool)
	act := workerActivity{fetched: map[string]struct{}{pageURL: {}}}
	for _, checked := range []bool{false, true} {
		view := newJudgeView(Config{ForeignNodes: []string{"web-researcher-2"}, judgePagesChecked: checked}, act)
		ctx := &judgeToolCtx{StrictContextMock: adkagent.NewStrictContextMock(withJudgeView(context.Background(), view))}
		out, err := list.Run(ctx, map[string]any{})
		listing, _ := out["result"].(string)
		if err != nil || strings.Contains(listing, sibID) || !strings.Contains(listing, upID) || strings.Contains(listing, pageID(t, pageURL)) != !checked {
			t.Errorf("checked=%v: list_artifacts = %q (err %v), want the upstream document a sibling edited last, the self-fetched page only when unchecked, never %s", checked, listing, err, sibID)
		}
		if _, err := read.Run(ctx, map[string]any{"id": sibID}); err == nil || !strings.Contains(err.Error(), "not found") {
			t.Errorf("checked=%v: read of a sibling artifact: err = %v, want not found", checked, err)
		}
		out, err = read.Run(ctx, map[string]any{"id": pageID(t, pageURL)})
		if body, _ := out["result"].(string); checked != (err != nil && strings.Contains(err.Error(), "code already checked")) || (!checked && len(body) < 16_000) {
			t.Errorf("checked=%v: page read = %d chars, err %v", checked, len(body), err)
		}
		if out, err := read.Run(ctx, map[string]any{"id": upID}); err != nil || out["result"] != "upstream notes" {
			t.Errorf("checked=%v: upstream read = %v, %v, want it unaffected", checked, out["result"], err)
		}
	}
}

// TestJudgeArtifactTools_InputsStayReadable: bytes inputs (pr-tutor's diff hunks, code-reviewer's
// PR files) read whole and repeatedly whether or not the round's pages were checked.
func TestJudgeArtifactTools_InputsStayReadable(t *testing.T) {
	rc, _, _, _ := seedScopedChat(t)
	tools, err := NewJudgeArtifactTools(rc)
	if err != nil {
		t.Fatal(err)
	}
	read := tools[1].(runnableTool)
	bytesID, err := recordstore.IdentityFor(kindBytes, nil, "files")
	if err != nil {
		t.Fatal(err)
	}
	for _, checked := range []bool{false, true} {
		ctx := &judgeToolCtx{StrictContextMock: adkagent.NewStrictContextMock(withJudgeView(context.Background(), newJudgeView(Config{judgePagesChecked: checked}, workerActivity{})))}
		for i := 1; i <= 6; i++ {
			out, err := read.Run(ctx, map[string]any{"id": bytesID})
			if body, _ := out["result"].(string); err != nil || len(body) < 16_000 {
				t.Fatalf("checked=%v: read %d = %d chars, err %v, want the whole ~20KB body", checked, i, len(body), err)
			}
		}
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

// fetchingWorker fetches url once, then answers.
type fetchingWorker struct{ url, answer string }

func (fetchingWorker) Name() string { return "fetching-worker" }

func (m fetchingWorker) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		for _, c := range req.Contents {
			for _, p := range c.Parts {
				if p.FunctionResponse != nil && p.FunctionResponse.Name == "web_fetch" {
					yield(stubText(m.answer), nil)
					return
				}
			}
		}
		yield(stubCall("web_fetch", map[string]any{"urls": []any{m.url}}), nil)
	}
}

type stubFetchArgs struct {
	URLs []string `json:"urls"`
}

type stubFetchResult struct {
	Results []map[string]string `json:"results"`
}

// TestRunGatedRefine_JudgePromptCarriesCitedEvidence: with specifics_supported declared, the
// verify tier runs before the judge and a specific its page contradicts reaches the judge prompt.
func TestRunGatedRefine_JudgePromptCarriesCitedEvidence(t *testing.T) {
	u := "https://example.test/survey"
	fetch, err := functiontool.New[stubFetchArgs, stubFetchResult](functiontool.Config{Name: "web_fetch", Description: "Fetch pages."},
		func(_ adkagent.Context, a stubFetchArgs) (stubFetchResult, error) {
			return stubFetchResult{Results: []map[string]string{{"url": u, "text": "stored"}}}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	unsupported := `{"items":[{"n":1,"state":"unsupported","quote":"users rose 30% in 2024"},{"n":2,"state":"supported","quote":"users rose 30% in 2024"}]}`
	var verifierCalls []string
	cfg := Config{JudgeRounds: 1, Threshold: 0.5, Rubric: "score the answer 0-10", RecordReader: fakeLoader{pageID(t, u): secondLookPage},
		RubricSpecs: map[string]criterionSpec{specificsSupportedCriterion: {Deterministic: true}},
		JudgeModel:  seqLLM{answers: []string{unsupported}, prompts: &verifierCalls}}
	judgeStub := &judgePromptCapturingModel{}
	runGatedStub(t, fetchingWorker{url: u, answer: "Users rose 25% in 2024 ([survey](" + u + "))."}, []tool.Tool{fetch}, NewJudgeFactory(judgeStub, nil, nil), cfg)
	if len(judgeStub.prompts) == 0 {
		t.Fatal("judge was never called")
	}
	first := judgeStub.prompts[0]
	ev, ans := strings.Index(first, "CITED EVIDENCE"), strings.Index(first, "Answer to judge:")
	if ev < 0 || ans < ev || !strings.Contains(first[ev:ans], `"25%"`) || !strings.Contains(first[ev:ans], "1 matched") || strings.Contains(first[ev:ans], `"2024"`) {
		t.Errorf("judge prompt lacks the unconfirmed specific (and only it) ahead of the answer:\n%s", first)
	}
}

// runGatedStub runs one gated node whose worker is workerModel with tools.
func runGatedStub(t *testing.T, workerModel model.LLM, tools []tool.Tool, judge JudgeFactory, cfg Config) GateResult {
	t.Helper()
	worker, err := llmagent.New(llmagent.Config{Name: "web-researcher", Model: workerModel, Tools: tools, Description: "researcher", Instruction: "Answer."})
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
	reads := []string{strings.Repeat("a", 3000), strings.Repeat("b", 3000), "cc"}
	c := judgeReadCounters{reads: &reads}
	if got := priorReadsSection(c, 100); got != "" {
		t.Errorf("no room: section = %.60q, want none", got)
	}
	got := priorReadsSection(c, 4000)
	if len(got) > 4000 || strings.Contains(got, "bbb") || !strings.Contains(got, "\ncc\n") || !strings.Contains(got, "(1 reads not shown: no room)") {
		t.Errorf("section = %d chars, want the first and last reads and a note for the one that did not fit", len(got))
	}
}

// TestRecordRead_NewestDeliverableWindowFirst: a later window of the deliverable replaces the
// earlier one and stays ahead of every other read.
func TestRecordRead_NewestDeliverableWindowFirst(t *testing.T) {
	s := &judgeRoundState{produced: map[string]bool{"text:report": true}}
	for i, c := range []struct{ id, body string }{{"text:report", "window one"}, {"web_page:p", "page"}, {"text:report", "window two"}} {
		call := fmt.Sprintf("c%d", i)
		s.noteCall(&genai.FunctionCall{ID: call, Name: "read_artifact", Args: map[string]any{"id": c.id, "offset": float64(i)}})
		s.recordRead(&genai.FunctionResponse{ID: call, Name: "read_artifact", Response: map[string]any{"result": c.body}})
	}
	got := strings.Join(s.reads, "|")
	if len(s.reads) != 2 || !strings.HasPrefix(s.reads[0], `read_artifact({"id":"text:report","offset":2})`) || strings.Contains(got, "window one") {
		t.Errorf("reads = %q, want the newest deliverable window first and the page read kept", got)
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

// deliverableRereadJudge reads the deliverable and a page, leaves a criterion unscored, then
// captures the re-judge's prompt.
type deliverableRereadJudge struct {
	calls  int32
	second string
}

func (*deliverableRereadJudge) Name() string { return "deliverable-reread-judge" }

func (j *deliverableRereadJudge) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		crit := map[string]any{"shortfall": "fine"}
		switch atomic.AddInt32(&j.calls, 1) {
		case 1:
			yield(stubCall("read_artifact", map[string]any{"id": "web_page:p"}), nil)
			return
		case 2:
			yield(stubCall("read_artifact", map[string]any{"id": "text:report"}), nil)
			return
		case 3:
		default:
			j.second = stubAllText(req)
			crit["score"] = 3.0
		}
		raw, _ := json.Marshal(map[string]any{"score": 3.0, "criteria": map[string]any{"accuracy": crit}, "feedback": ""})
		yield(stubText(string(raw)), nil)
	}
}

// TestReJudge_SeedsWholeDeliverableRead: the deliverable read reaches the retry whole and
// first; another read is cut to the page cap.
func TestReJudge_SeedsWholeDeliverableRead(t *testing.T) {
	report, page := strings.Repeat("r", 20_000), strings.Repeat("p", 20_000)
	read, err := functiontool.New[spyReadArtifactArgs, string](functiontool.Config{Name: "read_artifact", Description: "Read an artifact."},
		func(_ adkagent.Context, a spyReadArtifactArgs) (string, error) {
			if a.ID == "text:report" {
				return report, nil
			}
			return page, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	judge := &deliverableRereadJudge{}
	cfg := Config{Rubric: "score 0-3", JudgeMaxIterations: 6, Threshold: 0.6, JudgeArtifactTools: []tool.Tool{read},
		RubricSpecs: map[string]criterionSpec{"accuracy": {Name: "accuracy"}}}
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Write the report"}}}
	act := workerActivity{artifactsWritten: []string{"text:report"}}
	if _, err := runJudgeAgent(t.Context(), NewJudgeFactory(judge, nil, nil), cfg, q, "see text:report", act, nil, nil, func(*genai.Part) bool { return true }); err != nil {
		t.Fatal(err)
	}
	r, p := strings.Index(judge.second, report), strings.Index(judge.second, strings.Repeat("p", 100))
	if r < 0 || p < r || strings.Contains(judge.second, page) {
		t.Errorf("re-judge prompt: deliverable at %d, page at %d, page whole %v - want the whole deliverable first and the page cut", r, p, strings.Contains(judge.second, page))
	}
}

// TestBoundJudgeArtifactRead_ReplayHidesCheckedPages: replay's REST tools hide the pages the
// round's verify tier read, as live does, and leave everything else alone.
func TestBoundJudgeArtifactRead_ReplayHidesCheckedPages(t *testing.T) {
	ctx := withJudgeView(context.Background(), newJudgeView(Config{judgePagesChecked: true}, workerActivity{}))
	body := []byte(strings.Repeat("x", 20_000))
	if _, err := BoundJudgeArtifactRead(ctx, "web_page:p", body, "", 0, 0); err == nil || !JudgeHidesArtifact(ctx, "web_page:p") {
		t.Errorf("checked page read err = %v, want the refusal", err)
	}
	if got, err := BoundJudgeArtifactRead(ctx, "bytes:files", body, "", 0, 0); err != nil || len(got) != len(body) || JudgeHidesArtifact(ctx, "bytes:files") {
		t.Errorf("bytes read = %d chars, err %v, want the whole body", len(got), err)
	}
	if got, err := BoundJudgeArtifactRead(context.Background(), "web_page:p", body, "", 0, 0); err != nil || len(got) > judgeArtifactReadCap {
		t.Errorf("outside a judge round: %d chars, err %v", len(got), err)
	}
}

// versionsFailingStore fails every revision listing while fail is set.
type versionsFailingStore struct {
	*revisionMetaStore
	fail atomic.Bool
}

func (s *versionsFailingStore) Versions(ctx context.Context, req *artifact.VersionsRequest) (*artifact.VersionsResponse, error) {
	if s.fail.Load() {
		return nil, errors.New("store unavailable")
	}
	return s.revisionMetaStore.Versions(ctx, req)
}

// TestJudgeView_FailedHistoryLookupNotCached: a transient store error hides an upstream
// document for that call only; the next list sees it.
func TestJudgeView_FailedHistoryLookupNotCached(t *testing.T) {
	svc := &versionsFailingStore{revisionMetaStore: &revisionMetaStore{Service: artifact.InMemoryService(), meta: map[string][]byte{}}}
	rc, _, upID, _ := seedScopedChatOn(t, svc)
	tools, err := NewJudgeArtifactTools(rc)
	if err != nil {
		t.Fatal(err)
	}
	list := tools[0].(runnableTool)
	ctx := &judgeToolCtx{StrictContextMock: adkagent.NewStrictContextMock(withJudgeView(context.Background(), newJudgeView(Config{ForeignNodes: []string{"web-researcher-2"}}, workerActivity{})))}
	svc.fail.Store(true)
	if out, _ := list.Run(ctx, map[string]any{}); strings.Contains(out["result"].(string), upID) {
		t.Fatal("test setup: the upstream document was visible without its history")
	}
	svc.fail.Store(false)
	if out, _ := list.Run(ctx, map[string]any{}); !strings.Contains(out["result"].(string), upID) {
		t.Errorf("after the store recovered, list_artifacts = %q, want %s", out["result"], upID)
	}
}

// cappedJudge spends every reply on reasoning (MAX_TOKENS, no verdict) unless the request asks for
// low thinking; levels records each call's thinking level ("" when none was sent).
type cappedJudge struct{ levels []string }

func (*cappedJudge) Name() string { return "capped-judge" }

func (j *cappedJudge) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	level := ""
	if req.Config != nil && req.Config.ThinkingConfig != nil {
		level = string(req.Config.ThinkingConfig.ThinkingLevel)
	}
	j.levels = append(j.levels, level)
	return func(yield func(*model.LLMResponse, error) bool) {
		if level == string(genai.ThinkingLevelLow) {
			yield(stubText(`{"score": 3, "criteria": {"accuracy": {"reason": "ok", "score": 3}}, "feedback": ""}`), nil)
			return
		}
		yield(&model.LLMResponse{Content: &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{{Text: "Let me weigh"}}}, FinishReason: genai.FinishReasonMaxTokens, TurnComplete: true}, nil)
	}
}

// TestRetryNoVerdict_CappedRetriesWithLowThinking: a round whose reasoning ate the output cap is
// not nudged on the same settings, and its retry drops a configured medium/high effort to low;
// an unset level is never forced on (some endpoints reject reasoning_effort).
func TestRetryNoVerdict_CappedRetriesWithLowThinking(t *testing.T) {
	q := &genai.Content{Role: "user", Parts: []*genai.Part{{Text: "Research X"}}}
	for _, tc := range []struct {
		level  string
		want   string
		passes bool
	}{{"medium", "MEDIUM,LOW", true}, {"", ",", false}} {
		judge := &cappedJudge{}
		cfg := Config{Rubric: "score 0-3", JudgeMaxIterations: 6, JudgeThinkingLevel: tc.level}
		v, err := runJudgeAgent(t.Context(), NewJudgeFactory(judge, nil, nil), cfg, q, "done.", workerActivity{}, nil, nil, func(*genai.Part) bool { return true })
		if got := strings.Join(judge.levels, ","); got != tc.want || (err == nil) != tc.passes || (tc.passes && v.Score != 1.0) {
			t.Errorf("level %q: calls %q, err %v, score %v; want calls %q", tc.level, got, err, v.Score, tc.want)
		}
		if !tc.passes && !errors.Is(err, ErrJudgeOutputCapped) {
			t.Errorf("level %q: err = %v, want ErrJudgeOutputCapped", tc.level, err)
		}
	}
}

// TestVerifier_SendsJudgeThinkingLevel: the verifier runs on the judge's model with its effort.
func TestVerifier_SendsJudgeThinkingLevel(t *testing.T) {
	judge := &cappedJudge{}
	c := secondLookCheck()
	Verifier{LLM: judge, ThinkingLevel: "low"}.VerifyChecks(context.Background(), []UnitCheck{c})
	Verifier{LLM: judge}.VerifyChecks(context.Background(), []UnitCheck{c})
	if got := strings.Join(judge.levels, ","); got != "LOW," {
		t.Errorf("verifier thinking levels = %q, want LOW then none", got)
	}
}

// TestRunGatedRefine_JudgeNotShownCheckedPages: once runJudge's verify tier read the cited page,
// the judge's own list_artifacts no longer offers it.
func TestRunGatedRefine_JudgeNotShownCheckedPages(t *testing.T) {
	u := "https://example.test/survey"
	rc := recordstore.New(&revisionMetaStore{Service: artifact.InMemoryService(), meta: map[string][]byte{}}, artifactref.AppName, "u1", "chat1")
	pageID(t, u)
	if _, _, err := rc.SaveBlob(context.Background(), webPageKind, []byte(secondLookPage), "text/markdown", u, recordstore.Lineage{SourceURL: u}); err != nil {
		t.Fatal(err)
	}
	tools, err := NewJudgeArtifactTools(rc)
	if err != nil {
		t.Fatal(err)
	}
	fetch, err := functiontool.New[stubFetchArgs, stubFetchResult](functiontool.Config{Name: "web_fetch", Description: "Fetch pages."},
		func(_ adkagent.Context, a stubFetchArgs) (stubFetchResult, error) {
			return stubFetchResult{Results: []map[string]string{{"url": u, "text": "stored"}}}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	var prompts []string
	cfg := Config{JudgeRounds: 1, Threshold: 0.5, Rubric: "score 0-10", RecordReader: rc, JudgeArtifactTools: tools,
		RubricSpecs: map[string]criterionSpec{specificsSupportedCriterion: {Deterministic: true}},
		JudgeModel:  seqLLM{answers: []string{`{"items":[{"n":1,"state":"supported","quote":"users rose 30% in 2024"},{"n":2,"state":"supported","quote":"users rose 30% in 2024"}]}`}, prompts: &prompts}}
	listing := "(not called)"
	runGatedStub(t, fetchingWorker{url: u, answer: "Users rose 30% in 2024 ([survey](" + u + "))."}, []tool.Tool{fetch}, NewJudgeFactory(listingJudge{listing: &listing}, nil, nil), cfg)
	if len(prompts) == 0 || listing == "(not called)" || strings.Contains(listing, pageID(t, u)) {
		t.Errorf("verifier calls %d; judge list_artifacts = %q, want the checked page left out", len(prompts), listing)
	}
}
