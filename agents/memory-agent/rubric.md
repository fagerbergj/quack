# Candidate quality bar

Apply this bar to every fact before you emit it as a candidate.
When in doubt, leave it out - a missed memory costs nothing until it recurs and gets caught next time; a bad one pollutes every future turn.

A candidate qualifies only if it is **all** of the following:

- **Explicitly stated, not inferred.** The user said it, plainly, in the
  message. Do not extrapolate a general rule from a specific one-off request,
  and do not guess at a preference from tone, word choice, or brevity alone.
- **Durable, not transient.** It describes how the user generally works or
  communicates, not something scoped to this one task, this one PR, or this
  one message ("do it quickly this time" is transient; "always keep it quick"
  is durable).
- **A genuine work/communication preference, goal, or standing constraint** -
  matching the categories in the "what to remember" guidance above (verbosity,
  PR-vs-comment, proceed-vs-ask, a hard limit, a recurring piece of context
  about who they are or what they're working toward). Ordinary task content
  (what they want built, reviewed, or answered right now) is never a candidate.
- **Non-sensitive.** Skip health, legal, financial, political, or other
  sensitive personal information even if the user stated it - remembering it
  was never asked for and isn't this hook's job.
- **One fact per candidate.** Split a message that states two distinct
  durable facts into two candidates; never merge them into one run-on
  sentence.
- **Normalized for dedup.** Written the same stable, third-person way a
  restatement of the same fact would be written, so the consolidation pass
  recognizes it as the same memory rather than a near-duplicate.

A message with nothing meeting every point above yields **no candidates** -
not a weakened or hedged one.
