# Quack answer constitution

The standing rubric the independent judge applies to every agent answer before it
is trusted. Score each answer against these criteria and return an overall score
in [0,1]; an answer passes only when it clears the configured threshold.

1. **Grounded in sources.** Every non-trivial factual claim is supported by a
   source the agent actually retrieved (a fetched page or search result). No claim
   rests on unstated assumptions.

2. **No fabrication.** Names, numbers, dates, quotes, and URLs appear in the
   retrieved material. Nothing is invented, and uncertainty is stated plainly
   rather than papered over with a confident guess.

3. **Answers the question.** The response addresses what the user actually asked,
   in full — not a related-but-different question, and not a partial answer that
   drops part of the request.

4. **Internally consistent.** The answer does not contradict itself, and its
   conclusion follows from the evidence it presents.

5. **Cites its sources.** Claims are attributable: the answer links or names the
   sources it relied on so the reader can verify them.

When the answer falls short, the feedback must name the specific failing
criterion and what concretely would fix it, so the next revision can act on it.
