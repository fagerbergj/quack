package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/fagerbergj/quack/internal/schema"
)

// DecisionFilter narrows which decision entries the server returns.
type DecisionFilter struct {
	Since time.Time
	Point string
	Chats []string
}

// PointStats is one decision point's comparison of the model against quack's own logic.
type PointStats struct {
	Point string `json:"point"`
	N     int    `json:"n"`
	// Unavailable: calls that produced no answer (service down, cold, timed out).
	Unavailable int `json:"unavailable"`
	Answered    int `json:"answered"`
	Confident   int `json:"confident"`
	// Compared: answered calls that also carry a baseline; Agree: those where top == baseline.
	Compared  int      `json:"compared"`
	Agree     int      `json:"agree"`
	Coverage  *float64 `json:"coverage,omitempty"`
	Agreement *float64 `json:"agreement,omitempty"`
	// ConfidentDisagreements is the count; the list is DisagreementList.
	ConfidentDisagreements int            `json:"confident_disagreements"`
	DisagreementList       []Disagreement `json:"disagreement_list,omitempty"`
	MeanLatencyMS          float64        `json:"mean_latency_ms"`
	P50LatencyMS           float64        `json:"p50_latency_ms"`
	P50InputTokens         float64        `json:"p50_input_tokens"`
	// Confusion: top -> baseline -> count.
	Confusion map[string]map[string]int `json:"confusion,omitempty"`
}

// Disagreement is a confident answer that differs from quack's own decision.
type Disagreement struct {
	Chat     string  `json:"chat"`
	Node     string  `json:"node,omitempty"`
	Top      string  `json:"top"`
	Baseline string  `json:"baseline"`
	TopP     float64 `json:"top_p"`
}

// DecisionReport is `quack decisions report`'s result.
type DecisionReport struct {
	Points []PointStats `json:"points"`
	Totals PointStats   `json:"totals"`
	// Skipped: entries the server could not decode; Truncated: the server's limit dropped the oldest.
	Skipped   int  `json:"skipped,omitempty"`
	Truncated bool `json:"truncated,omitempty"`
}

func deref[T any](p *T) (v T) {
	if p != nil {
		v = *p
	}
	return v
}

func ratio(num, den int) *float64 {
	if den == 0 {
		return nil
	}
	r := float64(num) / float64(den)
	return &r
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := slices.Sorted(slices.Values(v))
	m := len(s) / 2
	if len(s)%2 == 1 {
		return s[m]
	}
	return (s[m-1] + s[m]) / 2
}

type pointAcc struct {
	PointStats
	latencies, tokens []float64
}

func (a *pointAcc) add(r schema.DecisionRecord) {
	a.N++
	if r.Outcome == "unavailable" || deref(r.Error) != "" || deref(r.Top) == "" {
		a.Unavailable++
		return
	}
	top, base := deref(r.Top), deref(r.Baseline)
	a.Answered++
	a.latencies = append(a.latencies, r.LatencyMs)
	a.tokens = append(a.tokens, float64(deref(r.InputTokens)))
	if r.Confident {
		a.Confident++
	}
	if base == "" {
		return
	}
	a.Compared++
	if a.Confusion == nil {
		a.Confusion = map[string]map[string]int{}
	}
	if a.Confusion[top] == nil {
		a.Confusion[top] = map[string]int{}
	}
	a.Confusion[top][base]++
	if top == base {
		a.Agree++
	} else if r.Confident {
		a.ConfidentDisagreements++
		a.DisagreementList = append(a.DisagreementList, Disagreement{Chat: r.ChatId, Node: deref(r.NodeId), Top: top, Baseline: base, TopP: deref(r.TopP)})
	}
}

func (a *pointAcc) finish() PointStats {
	s := a.PointStats
	s.Coverage, s.Agreement = ratio(s.Confident, s.Answered), ratio(s.Agree, s.Compared)
	s.P50LatencyMS, s.P50InputTokens = median(a.latencies), median(a.tokens)
	if len(a.latencies) > 0 {
		var sum float64
		for _, l := range a.latencies {
			sum += l
		}
		s.MeanLatencyMS = sum / float64(len(a.latencies))
	}
	return s
}

// BuildDecisionReport aggregates records per point, points sorted by id.
func BuildDecisionReport(recs []schema.DecisionRecord) DecisionReport {
	byPoint := map[string]*pointAcc{}
	total := &pointAcc{PointStats: PointStats{Point: "total"}}
	for _, r := range recs {
		a := byPoint[r.Point]
		if a == nil {
			a = &pointAcc{PointStats: PointStats{Point: r.Point}}
			byPoint[r.Point] = a
		}
		a.add(r)
		total.add(r)
	}
	rep := DecisionReport{Totals: total.finish()}
	rep.Totals.Confusion, rep.Totals.DisagreementList = nil, nil
	for _, a := range byPoint {
		rep.Points = append(rep.Points, a.finish())
	}
	sort.Slice(rep.Points, func(i, j int) bool { return rep.Points[i].Point < rep.Points[j].Point })
	return rep
}

// RunDecisionsReport is `quack decisions report`.
func RunDecisionsReport(ctx context.Context, out io.Writer, server string, f DecisionFilter, asJSON bool) error {
	list, err := fetchDecisions(ctx, server, f, false)
	if err != nil {
		return err
	}
	rep := BuildDecisionReport(list.Data)
	rep.Skipped, rep.Truncated = deref(list.Skipped), deref(list.Truncated)
	if asJSON {
		return WriteJSON(out, rep)
	}
	return writeReportTable(out, rep)
}

func fetchDecisions(ctx context.Context, server string, f DecisionFilter, withState bool) (schema.DecisionList, error) {
	c, err := NewClient(ctx, server)
	if err != nil {
		return schema.DecisionList{}, err
	}
	list, err := c.ListDecisions(ctx, f, withState)
	if errors.Is(err, ErrNotFound) {
		return list, fmt.Errorf("no ledger store is configured on this server")
	}
	return list, err
}

func pct(p *float64) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f%%", *p*100)
}

func statsRow(s PointStats) string {
	return fmt.Sprintf("%s\t%d\t%d\t%s\t%s\t%d\t%.0f\t%.0f\t%.0f\n", s.Point, s.N, s.Unavailable, pct(s.Coverage), pct(s.Agreement),
		s.ConfidentDisagreements, s.MeanLatencyMS, s.P50LatencyMS, s.P50InputTokens)
}

func writeReportTable(out io.Writer, rep DecisionReport) error {
	if len(rep.Points) == 0 {
		_, err := fmt.Fprintln(out, "No decisions recorded.")
		return err
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "POINT\tN\tUNAVAIL\tCOVERAGE\tAGREEMENT\tCONF-DISAGREE\tMEAN MS\tP50 MS\tP50 TOKENS")
	for _, s := range rep.Points {
		fmt.Fprint(tw, statsRow(s))
	}
	fmt.Fprint(tw, statsRow(rep.Totals))
	if err := tw.Flush(); err != nil {
		return err
	}
	for _, s := range rep.Points {
		if err := writePointDetail(out, s); err != nil {
			return err
		}
	}
	if rep.Truncated {
		fmt.Fprintln(out, "note: server limit reached; the oldest decisions were dropped (narrow with --since)")
	}
	if rep.Skipped > 0 {
		fmt.Fprintf(out, "note: %d entries skipped (undecodable payload)\n", rep.Skipped)
	}
	return nil
}

func writePointDetail(out io.Writer, s PointStats) error {
	if len(s.Confusion) == 0 && len(s.DisagreementList) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n%s: rows = model top, columns = baseline\n", s.Point)
	opts := confusionOptions(s.Confusion)
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "\t%s\n", strings.Join(opts, "\t"))
	for _, top := range opts {
		fmt.Fprint(tw, top)
		for _, base := range opts {
			fmt.Fprintf(tw, "\t%d", s.Confusion[top][base])
		}
		fmt.Fprintln(tw)
	}
	_ = tw.Flush()
	for _, d := range s.DisagreementList {
		fmt.Fprintf(&b, "  confident disagreement: chat %s node %s top=%s baseline=%s top_p=%.2f\n", d.Chat, d.Node, d.Top, d.Baseline, d.TopP)
	}
	_, err := io.WriteString(out, b.String())
	return err
}

// confusionOptions is every option seen as a top or a baseline, sorted.
func confusionOptions(c map[string]map[string]int) []string {
	seen := map[string]bool{}
	for top, row := range c {
		seen[top] = true
		for base := range row {
			seen[base] = true
		}
	}
	return slices.Sorted(maps.Keys(seen))
}
