package agent

import (
	"context"
	"iter"
	"log"
	"strings"

	"google.golang.org/adk/model"
)

// compactingModel wraps a model.LLM so that when a request is rejected for
// exceeding the context window, it summarizes the older turns and retries —
// REACTIVELY, driven by the server's own 400 rather than a token estimate that we
// repeatedly got wrong. The server is the ground truth for "too big," so there's
// nothing to mis-predict: we only ever compact when a request actually overflows.
//
// summarizer is the RAW (unwrapped) model so the summary call inside compaction
// can't recurse back through this wrapper.
type compactingModel struct {
	model.LLM
	summarizer model.LLM
}

// WrapCompacting wraps m so context-overflow errors trigger summarize-and-retry.
func WrapCompacting(m model.LLM) model.LLM {
	return &compactingModel{LLM: m, summarizer: m}
}

// maxCompactRetries bounds how many times a single call is compacted+retried, so a
// pathological request (a tail that alone won't fit) can't loop forever.
const maxCompactRetries = 3

func (c *compactingModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		contents := req.Contents
		for attempt := 0; ; attempt++ {
			r := *req
			r.Contents = contents
			overflowed := false
			emitted := false
			for resp, err := range c.LLM.GenerateContent(ctx, &r, stream) {
				// A context-overflow 400 arrives before any content is streamed. Catch it
				// (only if nothing was emitted yet), compact, and retry — instead of
				// letting it surface as a swallowed error / empty node.
				if err != nil && !emitted && attempt < maxCompactRetries && isContextOverflowErr(err) {
					overflowed = true
					break
				}
				emitted = true
				if !yield(resp, err) {
					return
				}
			}
			if !overflowed {
				return
			}
			compacted, ok := compactContents(ctx, c.summarizer, contents)
			if !ok {
				// Can't shrink further — let the next attempt surface the real error.
				log.Printf("compaction: context overflow but could not compact further")
				for resp, err := range c.LLM.GenerateContent(ctx, &r, stream) {
					if !yield(resp, err) {
						return
					}
				}
				return
			}
			log.Printf("compaction: context overflow — summarized %d→%d turns and retrying (attempt %d)", len(contents), len(compacted), attempt+1)
			contents = compacted
		}
	}
}

// isContextOverflowErr reports whether err is a model-server rejection for the
// request exceeding the context window. llama.cpp says "exceeds the available
// context size"; other servers vary, so match broadly on the "context" + "exceed"
// (or "context length") shapes.
func isContextOverflowErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "exceeds the available context"):
		return true
	case strings.Contains(s, "context length"):
		return true
	case strings.Contains(s, "context") && strings.Contains(s, "exceed"):
		return true
	default:
		return false
	}
}
