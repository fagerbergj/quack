// clefbench asks a systemone endpoint "which element should I click to <goal>?" over captured web UIs
// and reports accuracy, latency and input tokens. Cases run one at a time: Clef serves a single slot.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/fagerbergj/quack/internal/config"
	"github.com/fagerbergj/quack/internal/decide"
)

type page struct {
	Image    string   `json:"image"`
	Title    string   `json:"title"`
	URL      string   `json:"url"`
	Elements []string `json:"elements"`
}

type benchCase struct {
	Page       string   `json:"page"`
	Goal       string   `json:"goal"`
	Candidates []string `json:"candidates"`
	Answer     string   `json:"answer"`
}

type suite struct {
	Pages map[string]page `json:"pages"`
	Cases []benchCase     `json:"cases"`
}

type result struct {
	Case        benchCase
	Top         string
	P           float64
	Latency     time.Duration
	InputTokens int
	Err         error
}

func main() {
	url := flag.String("url", "", "handler base URL; /v1/systemone is appended")
	model := flag.String("model", "clef", "request model field")
	mode := flag.String("mode", "text", "text (element list), image (screenshot only), or both")
	cases := flag.String("cases", "tools/clefbench/testdata/cases.json", "cases file; images resolve beside it")
	timeout := flag.Duration("timeout", 30*time.Second, "per-request timeout")
	actAt := flag.Float64("act-at", 0.9, "top probability a decide point would act on")
	flag.Parse()
	if *url == "" || !slices.Contains([]string{"text", "image", "both"}, *mode) {
		flag.Usage()
		os.Exit(2)
	}
	s, err := load(*cases)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	c := decide.NewClient(config.DecisionHandler{URL: *url, Model: *model, Timeout: *timeout})
	rs := run(context.Background(), c, s, filepath.Dir(*cases), *mode)
	report(os.Stdout, rs, *actAt)
}

func load(path string) (suite, error) {
	var s suite
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	return s, json.Unmarshal(b, &s)
}

// state builds one case's evidence: the element list (text), the screenshot (image), or both.
func state(p page, dir, mode string) (any, error) {
	st := map[string]any{"page": p.Title, "url": p.URL}
	if mode != "image" {
		st["interactive_elements"] = p.Elements
	}
	if mode == "text" {
		return st, nil
	}
	img, err := os.ReadFile(filepath.Join(dir, p.Image))
	if err != nil {
		return nil, err
	}
	mime := "image/jpeg"
	if filepath.Ext(p.Image) == ".png" {
		mime = "image/png"
	}
	return decide.WithImages{State: st, Images: []string{"data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img)}}, nil
}

func run(ctx context.Context, h decide.Handler, s suite, dir, mode string) []result {
	var rs []result
	for _, bc := range s.Cases {
		r := result{Case: bc}
		st, err := state(s.Pages[bc.Page], dir, mode)
		if err != nil {
			r.Err = err
			rs = append(rs, r)
			continue
		}
		criteria := make(map[string]any, len(bc.Candidates))
		for _, cand := range bc.Candidates {
			criteria[cand] = nil
		}
		q := map[string]decide.Question{"target": {Type: "choice", Instructions: "Which element should be clicked to " + bc.Goal + "?", Criteria: criteria}}
		start := time.Now()
		reply, err := h.Ask(ctx, st, q)
		r.Latency, r.InputTokens, r.Err = time.Since(start), reply.InputTokens, err
		for opt, p := range reply.Answers["target"] {
			if p > r.P {
				r.Top, r.P = opt, p
			}
		}
		rs = append(rs, r)
	}
	return rs
}

func report(w io.Writer, rs []result, actAt float64) {
	var correct, confRight, confWrong, answered, tokens int
	var lat []time.Duration
	for _, r := range rs {
		ok := r.Err == nil && r.Top == r.Case.Answer
		_, _ = fmt.Fprintf(w, "%-13s ok=%-5v p=%.2f %5dms %5dtok top=%q goal=%q err=%v\n", r.Case.Page, ok, r.P, r.Latency.Milliseconds(), r.InputTokens, r.Top, r.Case.Goal, r.Err)
		if r.Err != nil {
			continue
		}
		answered++
		tokens += r.InputTokens
		lat = append(lat, r.Latency)
		if ok {
			correct++
		}
		if r.P >= actAt && ok {
			confRight++
		} else if r.P >= actAt {
			confWrong++
		}
	}
	if answered == 0 {
		_, _ = fmt.Fprintf(w, "no answers out of %d cases\n", len(rs))
		return
	}
	slices.Sort(lat)
	var sum time.Duration
	for _, l := range lat {
		sum += l
	}
	_, _ = fmt.Fprintf(w, "accuracy %d/%d answered (%d cases); p>=%.2f: %d right, %d wrong; latency mean %dms p50 %dms; input tokens mean %d\n",
		correct, answered, len(rs), actAt, confRight, confWrong, (sum / time.Duration(answered)).Milliseconds(), lat[len(lat)/2].Milliseconds(), tokens/answered)
}
