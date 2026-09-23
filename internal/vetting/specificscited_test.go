package vetting

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/session"
)

type fakeLoader map[string]string

func (f fakeLoader) Latest(_ context.Context, id string) ([]byte, int, bool, error) {
	b, ok := f[id]
	return []byte(b), 1, ok, nil
}

func specificsCfg() Config {
	return Config{RubricSpecs: map[string]criterionSpec{specificsCitedCriterion: {Name: specificsCitedCriterion, Deterministic: true}}}
}

// twelve substantive specifics, the last three uncited
const citedDoc = `Prices rose 12.5% in 2024 ([a](https://x.test/a)). Revenue hit $4.2M ([b](https://x.test/b)).

- The fleet has 1,200 trucks and 3,400 drivers ([c](https://x.test/c)).
- Uptime was 99.9% on 2024-03-01 and 98.5% on 2024-04-01 ([d](https://x.test/d)).

Margins were 17.3% and headcount 2,750, with 41.5% remote.`

func TestSpecificsCitedRatioAndEvidence(t *testing.T) {
	c, ok := specificsCitedCriterionScore(context.Background(), citedDoc, workerActivity{}, specificsCfg(), nil)
	if !ok {
		t.Fatal("criterion absent on a document with a dozen specifics")
	}
	if c.Score < 0.7 || c.Score > 0.8 {
		t.Errorf("score = %.2f, want 9/12 = 0.75", c.Score)
	}
	if !strings.Contains(c.Reason, `uncited "17.3%"`) || len(c.Evidence) != 3 {
		t.Errorf("reason/evidence do not name the uncited specifics: %q %+v", c.Reason, c.Evidence)
	}
}

func TestSpecificsCitedReadsWrittenArtifactForPointerAnswer(t *testing.T) {
	act := workerActivity{artifactsWritten: []string{"report-1"}}
	load := fakeLoader{"report-1": citedDoc}
	c, ok := specificsCitedCriterionScore(context.Background(), "Done - see artifact report-1.", act, specificsCfg(), load)
	if !ok || c.Score < 0.7 {
		t.Errorf("pointer answer must be scored on the artifact it wrote: ok=%v score=%.2f", ok, c.Score)
	}
	if _, ok := specificsCitedCriterionScore(context.Background(), "Done - see artifact report-1.", act, specificsCfg(), nil); ok {
		t.Error("without a reader a pointer answer has too few specifics; the criterion must be absent, not a fail")
	}
}

func TestSpecificsCitedCountsReceivedResearchAsBacking(t *testing.T) {
	cfg := specificsCfg()
	cfg.UpstreamAnswers = "Upstream found margins of 17.3%, headcount 2,750 and 41.5% remote work."
	c, ok := specificsCitedCriterionScore(context.Background(), citedDoc, workerActivity{}, cfg, nil)
	if !ok || c.Score != 1 {
		t.Errorf("uncited figures present in the received research must count as backed: ok=%v score=%.2f %q", ok, c.Score, c.Reason)
	}
}

func TestSpecificsCitedAbsentWhenUndeclaredOrThin(t *testing.T) {
	if _, ok := specificsCitedCriterionScore(context.Background(), citedDoc, workerActivity{}, Config{}, nil); ok {
		t.Error("a rubric that does not declare specifics_cited must not get it")
	}
	if _, ok := specificsCitedCriterionScore(context.Background(), "Only 12.5% and $4M here.", workerActivity{}, specificsCfg(), nil); ok {
		t.Error("two specifics are too few for a ratio; the criterion must be absent")
	}
}

func TestSubstantiveSkipsSmallIntegersAndQuotes(t *testing.T) {
	units := FindUnits(`Week 4 saw 5 bench slots and "a great run" for 2 teams; 1,500 fans paid $20.`)
	var got []string
	for _, u := range units {
		for _, s := range u.Specifics {
			if substantive(s) {
				got = append(got, s.Value)
			}
		}
	}
	sort.Strings(got)
	if strings.Join(got, "|") != "$20|1,500" {
		t.Errorf("substantive specifics = %v, want 1,500 and $20 only", got)
	}
}

// TestArtifactWritesCreditedToOwnNodeOnly: the live scan covers the whole chat
// session; another node's write_artifact reply must not become this node's deliverable.
func TestArtifactWritesCreditedToOwnNodeOnly(t *testing.T) {
	sess := newTestSession(t,
		fnResp("1", "write_artifact", map[string]any{"result": "ok: id=text:mine revision=1"}),
		fnResp("2", "write_artifact", map[string]any{"result": "ok: id=text:theirs revision=1"}),
	)
	i := 0
	for ev := range sess.Events().All() {
		ev.NodeInfo = &session.NodeInfo{Path: []string{"web-researcher-1@run1", "web-researcher-2@run2"}[i]}
		i++
	}
	act := activityFromSessionAt(sess, "", "web-researcher-1")
	if strings.Join(act.artifactsWritten, ",") != "text:mine" {
		t.Errorf("artifactsWritten = %v, want only this node's", act.artifactsWritten)
	}
	if got := activityFromSessionAt(sess, "", "").artifactsWritten; len(got) != 2 {
		t.Errorf("without a node id every write counts, got %v", got)
	}
}

func TestReplayCreditsArtifactsFromNodeTurns(t *testing.T) {
	turn := func(id string) RawTurn {
		return RawTurn{Output: `{"role":"user","parts":[{"functionResponse":{"id":"1","name":"write_artifact","response":{"result":"ok: id=` + id + ` revision=1"}}}]}`}
	}
	rc := ReplayCase{NodeID: "n1", WorkerTurns: []RawTurn{turn("text:theirs"), turn("text:mine")}, NodeTurns: []RawTurn{turn("text:mine")}}
	act, err := rebuildActivity(context.Background(), rc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(act.artifactsWritten, ",") != "text:mine" {
		t.Errorf("artifactsWritten = %v, want the node's own turns only", act.artifactsWritten)
	}
}

// TestCitesSourcesGradesThePointedArtifact: a short reply naming its artifact
// is graded on the artifact's links, not reported as having none.
func TestCitesSourcesGradesThePointedArtifact(t *testing.T) {
	u := "https://example.test/fetched"
	act := workerActivity{fetched: map[string]struct{}{u: {}}, seen: map[string]string{}, paths: map[string]bool{}, artifactsWritten: []string{"text:report"}}
	cfg := Config{RecordReader: fakeLoader{"text:report": "Revenue was $4.2M ([filing](" + u + "))."}}
	det, _ := computeDeterministicCriteria(context.Background(), "Report updated in text:report.", act, cfg, "node-1", time.Time{})
	c, ok := det["cites_sources"]
	if !ok || c.Score != 1 {
		t.Fatalf("cites_sources = %+v ok=%v, want the artifact's fetched link graded 1", c, ok)
	}
}
