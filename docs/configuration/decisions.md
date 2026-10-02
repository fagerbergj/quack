# Decision points

A decision point is a named intercept point in quack. When it is enabled, quack hands the point's evidence (text an LLM already wrote) to a handler, and the point's policy acts on the handler's answer or only records it. The one handler kind today is a System One decision model: one forward pass over typed questions, returning calibrated probabilities instead of generated text. Any endpoint speaking the Clef/Kev/Jev `POST /v1/systemone` shape works.

Every point is off unless `decisions.points.<id>.enabled` is true. A disabled point makes no call at all. An enabled point never blocks or fails a run: if the handler is down, cold, slow, or the input is over its cap, the point gets "no decision" and fails open to quack's own logic, unless it is a guard declared `fail: closed`.

```yaml
decisions:
  handlers:
    clef:
      kind: systemone          # the only kind; default systemone
      url: http://llm-swap-media:11436/upstream/clef-27b   # quack appends /v1/systemone
      model: clef              # the request's model field; default: the handler's key
      timeout: 5s              # whole call, retry included; default 5s
      max_input_tokens: 8192   # skip inputs well over the server's cap; default 8192
  points:
    plan.accept:
      enabled: true
      handler: clef
      mode: observe            # observe | guard | decide; default observe
      act_at: 0.9              # top-answer probability guard/decide need to act; default 0.9
      timeout: 3s              # optional: a tighter cap than the handler's
      fail: open               # open | closed (guard only); default open
      questions:               # optional: override a built-in question's text
        accept:
          instructions: Should an independent reviewer accept this plan step?
          # criteria: {true: ..., false: ...}   # noul; choice takes {option: description}, score a list
    "ext:github/review.verdict":   # an extension's declared point
      enabled: true
      handler: clef
      mode: observe
```

## Handlers

`kind: systemone` sends `url` + `/v1/systemone`. The call is retried once on a connection error, a 429 or a 5xx (Clef answers 503 when one batch runs out of GPU memory). A 413 (input over the server's token cap) and a 422 (a request the server rejects) are never retried. A 422 is also logged at warn, since it means quack sent a malformed question.

`timeout` bounds the whole call, and a point's own `timeout` can only shorten it. Keep both short: a llama-swap upstream that is still loading (Clef's cold start is about six minutes) blocks until the timeout, and the point then records `unavailable`.

`max_input_tokens` is a pre-call guard. It estimates tokens at six bytes each, above English's average of about four, so it skips only inputs well over the cap. A borderline input goes to the server, whose 413 is authoritative.

## Points, namespaces and modes

`points` is keyed by point id. Core points have unprefixed ids (`plan.accept`) and are registered in code, each with its questions, a primary question, the modes its caller implements, and the step a `decide` outcome would replace. The `ext:` prefix is reserved for extensions: an extension declares its points in its plugin (the SDK's `DecisionPoints`: questions, primary question, restrictive answers, modes; no modes means `observe` only), each becomes `ext:<plugin>/<name>`, and an extension can only evaluate its own declared points. At boot, after the enabled extensions are built, a point that is neither registered nor declared fails startup, as does an enabled point that sets a mode its caller doesn't implement, overrides a question it doesn't ask, or fails closed without a restrictive answer. The one exception is a disabled `ext:` entry whose extension declares nothing, such as one for an extension that is turned off: it is accepted, since it cannot be told apart from a typo there.

| Mode | What the caller does with the primary question's top answer |
| --- | --- |
| `observe` | Nothing. The decision is recorded next to quack's own outcome. |
| `guard` | Applies it only when it is confident (top probability ≥ `act_at`) and is one of the point's restrictive answers. A guard can narrow what quack does, never widen it. With `fail: closed`, no decision restricts too. |
| `decide` | Uses it when confident, replacing the point's step; below `act_at` quack's own logic decides (`fallback`). |

A noul question's options are `true` and `false`; a score question's options are the level indexes `0`, `1`, ... The top answer is the most probable option, never a score's mean.

| Point | Modes | Questions | State |
| --- | --- | --- | --- |
| `plan.accept` | `observe` | `accept` (noul, primary): should a reviewer accept this plan step? | The user's request and the plan summary the plan judge sees. It runs beside the plan judge and never changes the judge's verdict. |

The [GitHub extension](../extensions/github.md) declares three points, all asked after a review is posted and never changing what it posts. Each finding's state is its path, line, text without its label, and the diff hunk it is anchored to; the verdict's state is the pull request's title and body, every finding, and the reviewer's notes.

| Point | Modes | Questions | Restrictive | Baseline |
| --- | --- | --- | --- | --- |
| `ext:github/finding.severity` | `observe` | `severity` (choice, primary): `blocking`, `suggestion`, `nit`, `question` | `blocking` | The finding's severity from quack's review, else its body label; not asked for an unlabeled finding. |
| `ext:github/finding.blocking` | `observe` | `blocking` (noul, primary): would merging with this finding unaddressed be a mistake? | `true` | Whether the label is `blocking`. |
| `ext:github/review.verdict` | `observe` | `verdict` (choice, primary): `approve`, `comment`, `request_changes` | `comment`, `request_changes` | The review's own verdict. |

Points are built once at boot, so changing `decisions:` needs a restart.

## Recording

Every call an enabled point makes is recorded, including ones that returned no decision:

- **Ledger.** A `decision` observation entry on the run's chat, with the node and round when the point ran inside one. It carries the point, mode, handler, outcome, top answer and its probability, and every question's probabilities. `confident` says whether the top answer reached `act_at`; it is recorded in every mode, so an observe run shows what guard or decide would have done. `baseline` is quack's own outcome for the same point, in the primary question's option space; it is absent when a `decide` outcome skipped the step that would have produced it. A `decide` point that replaces a step cannot record that step's real output on a `fallback`: the call is one-shot, so the caller's placeholder baseline is stored and the shadow comparison is not valid for that point. `skipped_step` names the step a `decide` outcome replaced and is null otherwise, so savings can be summed from the replaced step's recorded cost. The entry also carries request bytes, input tokens, server and client latency, any error, and the exact state and questions sent. It is exported with the rest of a chat's observations by `quack ledger export`.
- **Trace.** A `quack.decision` span under the span that ran the point (for `plan.accept`, `quack.plan.judge`). It carries the same fields as `quack.decision.*` attributes, minus the state and questions.

Outcomes are `observe`, `pass`, `restrict`, `act`, `fallback`, and `unavailable`.
