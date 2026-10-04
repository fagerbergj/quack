package vetting

import (
	"context"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/fagerbergj/quack/internal/decide"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/recordstore"
)

// answerAccept observes each judge round's pass/fail for a node's answer.
var answerAccept = decide.RegisterObserveNoul("answer.accept", "pass",
	"Does this answer fully and correctly satisfy the task, with claims supported by the evidence shown?", "answer_judge")

// researchSource observes, per page a web-researcher fetched, whether the page bore on its question.
var researchSource = decide.RegisterObserveNoul("research.source", "relevant",
	"Is this page relevant to answering the research question?", "page_read")

// Byte caps keep each state near 10 KB: under Clef's 4096-token cap even for code at ~3 bytes/token.
const (
	answerAcceptTaskMax     = 2000
	answerAcceptAnswerMax   = 6000
	answerAcceptSourcesMax  = 750 // per list: cited, fetched
	researchQuestionMax     = 1500
	researchTitleMax        = 200
	researchURLMax          = 400
	researchTextMax         = 6000 // ~1,500 tokens of page text
	researchSourceMaxPages  = 20
	researchSourceAgentName = "web-researcher"
)

type answerAcceptState struct {
	Task    string   `json:"task"`
	Answer  string   `json:"answer"`
	Cited   []string `json:"cited,omitempty"`
	Fetched []string `json:"fetched,omitempty"`
}

type researchSourceState struct {
	Question string `json:"question"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	Text     string `json:"text"`
}

// observeAnswer starts answer.accept for this round's answer; settle it with the round's verdict, "" for none.
func (j *judgeRounds) observeAnswer(ctx context.Context, act workerActivity) func(baseline string) <-chan struct{} {
	return j.cfg.Decisions.Observe(ctx, answerAccept.Point, answerAcceptState{
		Task:    decide.Clip(j.cfg.Task, answerAcceptTaskMax),
		Answer:  decide.Clip(j.answer, answerAcceptAnswerMax),
		Cited:   clipList(citationsIn(j.answer, referenceList(j.answer)), answerAcceptSourcesMax),
		Fetched: clipList(slices.Sorted(maps.Keys(act.fetched)), answerAcceptSourcesMax),
	})
}

// clipList keeps the leading items whose total length stays within n bytes.
func clipList(items []string, n int) []string {
	for i, it := range items {
		if n -= len(it); n < 0 {
			return items[:i]
		}
	}
	return items
}

// researchLane admits one research.source call across all nodes: each queued request at a one-slot
// server eats every other point's timeout. ponytail: one global lane; a per-handler queue if more bulk points appear.
var researchLane = make(chan struct{}, 1)

// observeSources asks research.source about each page a web-researcher fetched, once its answer is final.
// It only spawns a goroutine, which asks one page at a time through researchLane.
func (g *gateRun) observeSources(answer string, act workerActivity) {
	if g.cfg.Agent != researchSourceAgentName || len(act.fetched) == 0 || !g.cfg.Decisions.Enabled(researchSource.ID) {
		return
	}
	load := recordReader(g.cfg)
	if load == nil {
		return
	}
	cfg := g.cfg
	ctx := ledger.WithCoords(context.WithoutCancel(g.nodeCtx), ledger.Coords{ChatID: cfg.ChatID, Node: cfg.NodeID, Agent: cfg.Agent, User: cfg.User, Source: cfg.Source})
	go observePages(ctx, cfg.Decisions, load, cfg.Task, answer, slices.Sorted(maps.Keys(act.fetched)))
}

func observePages(ctx context.Context, d *decide.Decider, load PageLoader, question, answer string, urls []string) {
	skipped := max(len(urls)-researchSourceMaxPages, 0)
	urls = urls[:len(urls)-skipped]
	cited := map[string]bool{}
	for _, c := range citationsIn(answer, referenceList(answer)) {
		for _, v := range urlVariants(c) {
			cited[v] = true
		}
	}
	for i, u := range urls {
		id, err := recordstore.IdentityFor(webPageKind, "", u)
		if err != nil {
			continue
		}
		data, _, ok, err := load.Latest(ctx, id)
		if err != nil || !ok {
			continue
		}
		citesPage := strings.Contains(answer, id) || slices.ContainsFunc(urlVariants(u), func(v string) bool { return cited[v] })
		state := decide.Annotated{
			State: researchSourceState{Question: decide.Clip(question, researchQuestionMax), Title: decide.Clip(firstLine(string(data)), researchTitleMax),
				URL: decide.Clip(u, researchURLMax), Text: decide.Clip(string(data), researchTextMax)},
			Meta: map[string]any{"artifact": id, "page": i + 1, "pages": len(urls), "skipped": skipped},
		}
		researchLane <- struct{}{}
		<-d.Observe(ctx, researchSource.Point, state)(strconv.FormatBool(citesPage))
		<-researchLane
	}
}

// firstLine is the page's first non-blank line without heading marks, web_fetch's stand-in for a title.
func firstLine(text string) string {
	for ln := range strings.Lines(text) {
		if t := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(ln), "# ")); t != "" {
			return t
		}
	}
	return ""
}
