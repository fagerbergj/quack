package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// Candidate is a memory the agent staged (or that the orchestrator wants written
// directly). Metadata is free-form (e.g. {"kind": "source"}); the schema carries
// no use-case vocabulary. Metadata["bucket"] (repo|role|user - stage_memory's
// `bucket` argument) routes the write; anything else takes Scope's default.
type Candidate struct {
	Content  string
	Metadata map[string]string
}

// nowRFC3339 lets tests stamp deterministically; defaults to time.Now.
var nowRFC3339 = func() string { return time.Now().UTC().Format(time.RFC3339) }

// Commit vets, extracts, and consolidates memories into this collection,
// routing each one to the BUCKET it is about (see scope.go), and returns the
// number of points written or updated. It is the single gated writer: the
// gate calls it on a judge pass; the orchestrator calls it directly for user
// facts.
//
// Routing is explicit and cheap, never an LLM judgment: a staged candidate
// names its bucket (Metadata["bucket"]) and sc.writeBucket resolves it to a
// key, degrading repo → role → user when the caller has no repo context. The
// source answer's extraction goes to the caller's default bucket.
//
// Each bucket is consolidated separately, because a memory only ever
// competes with others about the SAME subject.
//
// ponytail: per-(bucket, collection) commits can race - two parallel commits of
// the same fact both ADD. Best-effort: the next commit's consolidation reconciles
// the dup. Add a per-key lock only if duplicate churn proves real.
func (s *Store) Commit(ctx context.Context, sc Scope, author string, staged []Candidate, sourceText string) (int, error) {
	if s.consolidator == nil {
		return 0, fmt.Errorf("memory: Commit on a store with no consolidator")
	}
	staged = dedupCandidates(staged) // collapse the same sentence staged across passes
	if len(staged) == 0 && strings.TrimSpace(sourceText) == "" {
		return 0, nil
	}

	// Group the staged candidates by the bucket they belong in; the answer's own
	// extraction rides the default bucket.
	byBucket := map[string][]Candidate{}
	for _, c := range staged {
		if b := sc.writeBucket(c.Metadata["bucket"]); b != "" {
			byBucket[b] = append(byBucket[b], c)
		}
	}
	def := sc.writeBucket("")
	if def == "" {
		// Nothing to key a write on (no repo, no role, no user): drop it rather than
		// invent a bucket. A memory nobody can address is worse than no memory.
		s.log.Debug("commit skipped: caller has no writable bucket", "author", author)
		return 0, nil
	}
	if strings.TrimSpace(sourceText) != "" {
		if _, ok := byBucket[def]; !ok {
			byBucket[def] = nil
		}
	}

	total := 0
	for _, bucket := range slices.Sorted(maps.Keys(byBucket)) { // deterministic order
		src := ""
		if bucket == def {
			src = sourceText
		}
		n, err := s.commitTo(ctx, bucket, author, byBucket[bucket], src)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// commitTo runs the vet + reconcile pass for ONE bucket and applies its operations.
func (s *Store) commitTo(ctx context.Context, bucket, author string, staged []Candidate, sourceText string) (int, error) {
	// Fetch existing memories in this bucket near the work to reconcile against.
	neighbours, err := s.neighbours(ctx, bucket, sourceText, staged)
	if err != nil {
		return 0, err
	}

	ops, err := s.decide(ctx, staged, sourceText, neighbours)
	if err != nil {
		return 0, err
	}
	if len(ops) == 0 {
		return 0, nil
	}

	// Only honour an UPDATE/DELETE id the consolidator was actually shown - a
	// hallucinated id would otherwise upsert an orphan point at an arbitrary id.
	valid := make(map[string]bool, len(neighbours))
	for _, n := range neighbours {
		valid[n.ID] = true
	}
	return s.apply(ctx, bucket, author, ops, valid)
}

// dedupCandidates drops candidates with duplicate trimmed content (cheap; saves
// the consolidator embedding/reasoning over the same sentence twice).
func dedupCandidates(in []Candidate) []Candidate {
	seen := make(map[string]bool, len(in))
	out := make([]Candidate, 0, len(in))
	for _, c := range in {
		key := strings.TrimSpace(c.Content)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, c)
	}
	return out
}

type neighbour struct {
	ID      string
	Content string
}

// op is one consolidation decision from the LLM.
type op struct {
	Action  string `json:"action"`  // ADD | UPDATE | DELETE | NOOP
	ID      string `json:"id"`      // existing memory id (UPDATE / DELETE)
	Content string `json:"content"` // memory text (ADD / UPDATE)
	Kind    string `json:"kind"`    // free-form tag stored in metadata
}

// maxProbeRunes caps the text embedded to find dedup neighbours. The probe only
// needs to be representative of the work, not complete - a research answer can be
// 10k+ chars, and embedding all of it on a CPU model costs seconds for no extra
// dedup value. The memories actually written are short atomic facts embedded in
// full.
const maxProbeRunes = 2000

// neighbourProbe builds the (capped) text whose nearest existing memories we
// reconcile against. Staged candidates go first (they're closest to what we'll
// write, so they survive truncation), followed by a prefix of the source answer.
func neighbourProbe(sourceText string, staged []Candidate) string {
	var b strings.Builder
	for _, c := range staged {
		b.WriteString(strings.TrimSpace(c.Content))
		b.WriteByte('\n')
	}
	b.WriteString(sourceText)
	probe := strings.TrimSpace(b.String())
	if r := []rune(probe); len(r) > maxProbeRunes {
		probe = string(r[:maxProbeRunes])
	}
	return probe
}

func (s *Store) neighbours(ctx context.Context, bucket, sourceText string, staged []Candidate) ([]neighbour, error) {
	probe := neighbourProbe(sourceText, staged)
	if probe == "" {
		return nil, nil
	}
	vecs, err := s.embed(ctx, []string{probe}, "commit-neighbours")
	if err != nil {
		return nil, fmt.Errorf("memory: embed for neighbours: %w", err)
	}
	if len(vecs) == 0 {
		return nil, nil
	}
	pts, err := s.idx.query(ctx, []string{bucket}, vecs[0], s.topK)
	if err != nil {
		return nil, fmt.Errorf("memory: neighbour query: %w", err)
	}
	out := make([]neighbour, 0, len(pts))
	for _, p := range pts {
		if p.Content != "" {
			out = append(out, neighbour{ID: p.ID, Content: p.Content})
		}
	}
	return out, nil
}

// decide runs the single consolidation pass and returns the operations to apply.
func (s *Store) decide(ctx context.Context, staged []Candidate, sourceText string, neighbours []neighbour) ([]op, error) {
	var user strings.Builder
	if len(staged) > 0 {
		user.WriteString("STAGED CANDIDATES:\n")
		for _, c := range staged {
			if k := c.Metadata["kind"]; k != "" {
				fmt.Fprintf(&user, "- [%s] %s\n", k, c.Content) // pass the agent's kind hint through
			} else {
				fmt.Fprintf(&user, "- %s\n", c.Content)
			}
		}
	}
	if strings.TrimSpace(sourceText) != "" {
		user.WriteString("\nFINAL ANSWER (extract additional durable tradecraft from it):\n")
		user.WriteString(sourceText)
		user.WriteString("\n")
	}
	user.WriteString("\nEXISTING MEMORIES (reconcile against these):\n")
	if len(neighbours) == 0 {
		user.WriteString("(none)\n")
	} else {
		for _, n := range neighbours {
			fmt.Fprintf(&user, "- id=%s: %s\n", n.ID, n.Content)
		}
	}

	sysPrompt, ok := consolidatePrompts[s.domain]
	if !ok {
		sysPrompt = consolidatePrompts["task"]
	}
	// /no_think disables reasoning on qwen/gemma-class models (the configured
	// consolidation model); harmless to others.
	req := &model.LLMRequest{
		Contents: []*genai.Content{{Role: "user", Parts: []*genai.Part{{Text: "/no_think " + user.String()}}}},
		Config: &genai.GenerateContentConfig{
			SystemInstruction: &genai.Content{Parts: []*genai.Part{{Text: sysPrompt}}},
			// JSON mode grammar-constrains the model to valid JSON, so a stray escape
			// (a bare `\s` from a regex/path in a memory) can't crash json.Unmarshal
			// with "invalid character 's' in string escape code". The prompt already
			// says "Reply with ONLY JSON", satisfying json_object mode's require-json rule.
			ResponseMIMEType: "application/json",
		},
	}
	var sb strings.Builder
	for resp, err := range s.consolidator.GenerateContent(ctx, req, false) {
		if err != nil {
			return nil, fmt.Errorf("memory: consolidation model: %w", err)
		}
		if resp.Content != nil {
			for _, part := range resp.Content.Parts {
				if !part.Thought && part.Text != "" {
					sb.WriteString(part.Text)
				}
			}
		}
	}
	var parsed struct {
		Ops []op `json:"ops"`
	}
	if err := json.Unmarshal([]byte(stripFences(sb.String())), &parsed); err != nil {
		return nil, fmt.Errorf("memory: parse consolidation output: %w", err)
	}
	return parsed.Ops, nil
}

// apply writes the operations into one bucket: ADD/UPDATE upsert a point (UPDATE
// keeps the existing id), DELETE removes one, NOOP is skipped. valid is the set of
// ids the consolidator was shown; an UPDATE/DELETE naming any other id is treated as
// a hallucination (UPDATE → fresh ADD, DELETE → dropped). Returns writes applied.
func (s *Store) apply(ctx context.Context, bucket, author string, ops []op, valid map[string]bool) (int, error) {
	var dels []string
	var writes []op
	for _, o := range ops {
		switch strings.ToUpper(strings.TrimSpace(o.Action)) {
		case "ADD", "UPDATE":
			if strings.TrimSpace(o.Content) != "" {
				if !valid[o.ID] {
					o.ID = "" // hallucinated/absent id → fresh ADD, not an orphan upsert
				}
				writes = append(writes, o)
			}
		case "DELETE":
			if o.ID != "" && valid[o.ID] {
				dels = append(dels, o.ID)
			}
		}
	}

	count := 0
	if len(writes) > 0 {
		texts := make([]string, len(writes))
		for i, o := range writes {
			texts[i] = o.Content
		}
		vecs, err := s.embed(ctx, texts, "commit-write")
		if err != nil {
			return 0, fmt.Errorf("memory: embed writes: %w", err)
		}
		ts := nowRFC3339()
		points := make([]point, 0, len(writes))
		for i, o := range writes {
			id := o.ID
			if strings.ToUpper(strings.TrimSpace(o.Action)) == "ADD" || id == "" {
				id = uuid.NewString()
			}
			points = append(points, point{
				ID:        id,
				Vector:    vecs[i],
				Content:   o.Content,
				Scope:     bucket,
				Author:    author,
				Timestamp: ts,
				Kind:      o.Kind,
			})
		}
		if err := s.idx.upsert(ctx, points); err != nil {
			return 0, err
		}
		count += len(points)
	}

	if len(dels) > 0 {
		if _, err := s.idx.remove(ctx, dels); err != nil {
			return 0, err
		}
		count += len(dels)
	}

	s.log.Debug("commit", "bucket", bucket, "author", author, "ops", len(ops), "writes", count)
	return count, nil
}

// consolidatePrompts holds the domain-specific consolidation system prompt. The
// reconcile mechanics (ADD/UPDATE/DELETE/NOOP + JSON shape) are identical; only
// the "what's worth keeping" framing differs by scope.
var consolidatePrompts = map[string]string{
	"task": "You maintain a team of agents' SHARED long-term memory about one subject - either a " +
		"repository (its conventions, build/test/lint commands, layout, where things are registered, " +
		"pre-existing failures) or a role's durable tradecraft (which sources proved authoritative and " +
		"for what, which were junk, tactics that worked, dead-ends). You are given STAGED candidates, the " +
		"agent's FINAL ANSWER, and the most similar EXISTING MEMORIES about this same subject.\n\n" +
		"Produce a set of operations. First VET: keep only durable knowledge worth recalling in future " +
		"unrelated tasks on this subject; drop anything volatile, request-specific, speculative, or not " +
		"clearly supported. Then RECONCILE each kept memory against the existing ones:\n" +
		"- ADD: genuinely new - provide content (one atomic sentence) and a kind (e.g. convention|command|layout|source|search|fetch|deadend).\n" +
		"- UPDATE: refines/supersedes an existing memory - provide its id plus the new content and kind.\n" +
		"- DELETE: an existing memory is now contradicted or obsolete - provide its id.\n" +
		"- NOOP: already covered - skip it.\n\n" +
		"Reply with ONLY JSON: {\"ops\":[{\"action\":\"ADD|UPDATE|DELETE|NOOP\",\"id\":\"\",\"content\":\"\",\"kind\":\"\"}]}. " +
		"Empty ops list if nothing is worth keeping.",

	"user": "You maintain durable facts ABOUT THE USER - who they are, their preferences, relationships, " +
		"possessions, goals, and hard limits - so the assistant can personalize. You are given STAGED " +
		"candidates (things the user revealed about themselves) and the most similar EXISTING MEMORIES.\n\n" +
		"Produce a set of operations. First VET: keep only durable facts the user actually stated about " +
		"themselves; drop transient/request-specific details and anything sensitive they did not ask to be " +
		"kept. Then RECONCILE each kept fact against the existing ones:\n" +
		"- ADD: genuinely new - provide content (one atomic sentence) and a kind (identity|preference|relationship|possession|goal|limit).\n" +
		"- UPDATE: the fact changed (moved, switched jobs, new preference) - provide the existing id plus new content and kind.\n" +
		"- DELETE: an existing fact is now contradicted - provide its id.\n" +
		"- NOOP: already known - skip it.\n\n" +
		"Reply with ONLY JSON: {\"ops\":[{\"action\":\"ADD|UPDATE|DELETE|NOOP\",\"id\":\"\",\"content\":\"\",\"kind\":\"\"}]}. " +
		"Empty ops list if nothing is worth keeping.",
}

// stripFences removes a leading ```json / ``` fence and trailing ``` if present,
// so a model that wraps its JSON still parses.
func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimPrefix(s, "json")
	s = strings.TrimPrefix(s, "JSON")
	if i := strings.LastIndex(s, "```"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
